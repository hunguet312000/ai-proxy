"""Extract protobuf-es descriptors out of the Cursor IDE bundle.

LiteRouter speaks Cursor's private agent protocol by hand-encoding protobuf against a
schema nobody publishes. When the IDE updates, a field number can move and requests
start failing in ways that never name the field — so this recovers the schema from the
IDE itself instead of from guesswork.

The IDE ships generated protobuf-es code, so every message is a
`VAR=A.makeMessageType("pkg.Name",()=>[ ...fields ])` call. Building var -> typeName
first lets `T:VAR` references in field lists resolve back to real type names.

    python3 tools/cursor-schema.py agent.v1.AgentRunRequest
    python3 tools/cursor-schema.py agent.v1.McpArgs agent.v1.ToolCall

Set LITEROUTER_CURSOR_BUNDLE to point at a different install. See
docs/cursor-protocol.md for what to compare the output against.
"""
import os
import re
import sys

BUNDLE = os.environ.get(
    'LITEROUTER_CURSOR_BUNDLE',
    '/Applications/Cursor.app/Contents/Resources/app/out/vs/workbench/workbench.desktop.main.js')
SCALARS = {1: 'double', 2: 'float', 3: 'int64', 4: 'uint64', 5: 'int32', 8: 'bool',
           9: 'string', 12: 'bytes', 13: 'uint32', 14: 'enum', 18: 'sint64'}


def load():
    return open(BUNDLE, encoding='utf-8', errors='replace').read()


def balanced(text, start):
    """Return the [...] slice beginning at start, respecting nesting and strings."""
    depth, index, in_string, quote, escaped = 0, start, False, '', False
    while index < len(text):
        char = text[index]
        if in_string:
            if escaped:
                escaped = False
            elif char == '\\':
                escaped = True
            elif char == quote:
                in_string = False
        elif char in '"\'`':
            in_string, quote = True, char
        elif char == '[':
            depth += 1
        elif char == ']':
            depth -= 1
            if depth == 0:
                return text[start:index + 1]
        index += 1
    return text[start:start + 4000]


def index_messages(source):
    """var -> [typeName], and typeName -> raw field-list source.

    Minified vars are reused across modules — rare (1 in 6056) but it lands on real
    types, so every candidate is kept and resolution picks by package later.
    """
    by_var, by_name = {}, {}
    pattern = re.compile(r'(\w+)\s*=\s*\w+\.makeMessageType\(\s*"([^"]+)"\s*,\s*\(\)\s*=>\s*\[')
    for match in pattern.finditer(source):
        var, name = match.group(1), match.group(2)
        by_var.setdefault(var, []).append(name)
        by_name[name] = balanced(source, match.end() - 1)
    return by_var, by_name


def resolve(var, table, package):
    """Pick the candidate type for a var, preferring the enclosing message's package."""
    candidates = table.get(var)
    if not candidates:
        return '?' + var
    if len(candidates) == 1:
        return candidates[0]
    same = [c for c in candidates if c.rsplit('.', 1)[0] == package]
    if len(same) == 1:
        return same[0]
    return candidates[0] + '  /* ambiguous: ' + ', '.join(candidates) + ' */'


def index_enums(source):
    by_var, by_name = {}, {}
    pattern = re.compile(r'(\w+)\s*=\s*\w+\.makeEnum\(\s*"([^"]+)"\s*,\s*\[')
    for match in pattern.finditer(source):
        var, name = match.group(1), match.group(2)
        by_var.setdefault(var, []).append(name)
        by_name[name] = balanced(source, match.end() - 1)
    return by_var, by_name


FIELD = re.compile(r'\{\s*no:\s*(\d+),\s*name:\s*"([A-Za-z0-9_]+)"(.*?)\}(?=\s*[,\]])', re.S)


def fields_of(raw, msg_vars, enum_vars, package):
    out = []
    depth = 0
    # Only take fields at the top level of this message's list.
    for match in FIELD.finditer(raw):
        prefix = raw[:match.start()]
        if prefix.count('[') - prefix.count(']') != 1:
            continue
        number, name, rest = int(match.group(1)), match.group(2), match.group(3)
        kind = re.search(r'kind:\s*"(\w+)"', rest)
        kind = kind.group(1) if kind else '?'
        target = re.search(r'T:\s*([A-Za-z0-9_$]+)', rest)
        target = target.group(1) if target else ''
        if kind == 'scalar':
            typename = SCALARS.get(int(target), 'T' + target) if target.isdigit() else target
        elif kind == 'message':
            typename = resolve(target, msg_vars, package)
        elif kind == 'enum':
            inner = re.search(r'T:\s*\w+\.getEnumType\((\w+)\)', rest)
            typename = resolve(inner.group(1) if inner else target, enum_vars, package)
        else:
            typename = target
        out.append({
            'no': number, 'name': name, 'kind': kind, 'type': typename,
            'repeated': 'repeated:!0' in rest.replace(' ', ''),
            'opt': 'opt:!0' in rest.replace(' ', ''),
            'oneof': (re.search(r'oneof:\s*"(\w+)"', rest) or [None, ''])[1],
        })
    return sorted(out, key=lambda f: f['no'])


def main():
    source = load()
    msg_vars, msg_raw = index_messages(source)
    enum_vars, enum_raw = index_enums(source)
    wanted = sys.argv[1:]
    seen = set()
    queue = list(wanted)
    while queue:
        name = queue.pop(0)
        if name in seen:
            continue
        seen.add(name)
        if name in enum_raw:
            values = re.findall(r'\{\s*no:\s*(-?\d+),\s*name:\s*"([A-Za-z0-9_]+)"', enum_raw[name])
            print(f'\nenum {name}')
            for number, value in values:
                print(f'  {number:>3} {value}')
            continue
        if name not in msg_raw:
            print(f'\n?? {name} not found')
            continue
        print(f'\nmessage {name}')
        for field in fields_of(msg_raw[name], msg_vars, enum_vars, name.rsplit('.', 1)[0]):
            flags = ''.join(['[]' if field['repeated'] else '', '?' if field['opt'] else ''])
            oneof = f"  oneof:{field['oneof']}" if field['oneof'] else ''
            print(f"  {field['no']:>3} {field['name']:<38} {field['type']}{flags}{oneof}")
            if field['kind'] in ('message', 'enum') and len(sys.argv) > 1 and '--deep' in wanted:
                queue.append(field['type'])


if __name__ == '__main__':
    main()
