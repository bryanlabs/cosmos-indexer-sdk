#!/usr/bin/env python3
"""Reject committed database credentials without printing any matched values."""
import argparse
import json
from pathlib import Path
import re
import subprocess

SENSITIVE = r'(?:DB_PASS|DB_PASSWORD|PGPASSWORD|POSTGRES_PASSWORD|DATABASE_PASSWORD)'
ENV_NAME = re.compile(r'[\"\']?\bname[\"\']?\s*:\s*[\"\']?(' + SENSITIVE + r')\b')
INLINE_VALUE = re.compile(r'[\"\']?\bvalue[\"\']?\s*:\s*([^\n,}]+)')
RULES = [
    ('password-assignment', re.compile(r'\b' + SENSITIVE + r'\s*=\s*([A-Za-z0-9+/][A-Za-z0-9_+/.=-]*)')),
    ('quoted-password-assignment', re.compile(r'\b' + SENSITIVE + r'\s*=\s*[\"\']([^\s\"\'$%{][^\"\'\n]*)[\"\']')),
    ('password-key', re.compile(r'(?m)^\s*[\"\']?' + SENSITIVE + r'[\"\']?\s*:\s*[\"\']?([^\s\"\'{}\n,]+)')),
    ('password-dsn', re.compile(r'\bpassword\s*=\s*[\"\']?([^\s\"\'\\]+)', re.I)),
    ('sql-password', re.compile(r'\bPASSWORD\s+\'([^\'\n]+)\'', re.I)),
    ('password-bearing-url', re.compile(r'postgres(?:ql)?://[^\s/:@]+:([^\s@]+)@', re.I)),
    ('password-config', re.compile(r'^\s*password\s*=\s*[\"\']([^\"\']+)[\"\']', re.M | re.I)),
    ('password-default', re.compile(r'env\(\s*[\"\'](?:DB_PASS|PGPASSWORD|POSTGRES_PASSWORD)[\"\']\s*,\s*[\"\']([^\"\']+)[\"\']')),
    ('password-argument', re.compile(r'--database\.password(?:\s*=\s*|\s+|[\"\']\s*,\s*)[\"\']?([^\s\\\"\'\],}]+)')),
    ('password-dict', re.compile(r'[\"\']' + SENSITIVE + r'[\"\']\s*:\s*[\"\']([^\"\']+)[\"\']')),
]


def dynamic(value):
    value = value.strip().strip('\"\'')
    return (not value or value.startswith(('$', '%', '{'))
            or value in ('null', 'None') or bool(re.fullmatch(SENSITIVE, value)))


def findings(text, suffix):
    result = []
    for field, pattern in RULES:
        for match in pattern.finditer(text):
            if field == 'password-dsn':
                start = text.rfind('\n', 0, match.start()) + 1
                end = text.find('\n', match.end())
                line = text[start:end if end != -1 else len(text)]
                if not re.search(r'\b(?:host|user|dbname)\s*=|DATABASE_URL|\bDSN\b', line, re.I):
                    continue
            if not dynamic(match.group(1)):
                result.append(field)
    json_text = '\n'.join(line for line in text.splitlines() if not line.lstrip().startswith('#'))
    parsed_json = None
    if suffix == '.json' or json_text.lstrip().startswith(('{', '[')):
        try:
            parsed_json = json.loads(json_text)
        except ValueError:
            if suffix == '.json':
                result.append('invalid-json')
    if parsed_json is not None:
        def walk(value):
            if isinstance(value, dict):
                if re.fullmatch(SENSITIVE, str(value.get('name', ''))) and 'value' in value:
                    secret_ref = value.get('valueFrom', {}).get('secretKeyRef', {})
                    deletes_old_literal = (value['value'] is None
                                           and bool(secret_ref.get('name'))
                                           and bool(secret_ref.get('key')))
                    if not deletes_old_literal:
                        result.append(value['name'])
                for key, child in value.items():
                    if re.fullmatch(SENSITIVE, key) and isinstance(child, str) and not dynamic(child):
                        result.append('password-key')
                    walk(child)
            elif isinstance(value, list):
                for child in value:
                    walk(child)
        walk(parsed_json)
    else:
        lines = text.splitlines()
        for i, line in enumerate(lines):
            name = ENV_NAME.search(line)
            if not name:
                continue
            # Find the owning YAML sequence item, regardless of key order.
            indent = len(line) - len(line.lstrip())
            start = i
            item_indent = indent
            if not line.lstrip().startswith('-') and '{' not in line:
                for previous in range(i - 1, -1, -1):
                    candidate = lines[previous]
                    depth = len(candidate) - len(candidate.lstrip())
                    if candidate.strip() and depth < indent:
                        if candidate.lstrip().startswith('-'):
                            start, item_indent = previous, depth
                        break
            end = i + 1
            while end < len(lines):
                candidate = lines[end]
                if candidate.strip() and len(candidate) - len(candidate.lstrip()) <= item_indent:
                    break
                end += 1
            block = '\n'.join(lines[start:end])
            value = INLINE_VALUE.search(block)
            if value:
                scalar = value.group(1).strip().strip('\"\'')
                deletes_old_literal = (scalar in ('null', '~')
                                       and 'valueFrom' in block and 'secretKeyRef' in block)
                if not deletes_old_literal:
                    result.append(name.group(1))
    return sorted(set(result))


def scan(root, staged=False):
    entries = subprocess.check_output(['git', 'ls-files', '--stage', '-z'], cwd=root).decode().split('\0')
    files = [entry.split('\t', 1)[1] for entry in entries
             if entry and not entry.startswith('160000 ')]
    results = []
    for name in filter(None, files):
        if name.endswith(('go.sum', 'go.mod')):
            continue
        path = root / name
        try:
            if staged:
                content = subprocess.check_output(['git', 'show', ':' + name], cwd=root).decode()
            else:
                content = path.read_text()
        except (UnicodeError, FileNotFoundError, IsADirectoryError):
            continue
        fields = findings(content, path.suffix)
        if fields:
            results.append({'path': name, 'fields': fields, 'count': len(fields)})
    return results


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument('--staged', action='store_true')
    args = parser.parse_args()
    rows = scan(args.root, args.staged)
    print(json.dumps({'files': len(rows), 'findings': rows}, indent=2))
    raise SystemExit(bool(rows))


if __name__ == '__main__':
    main()
