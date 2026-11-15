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
    ('quoted-password-assignment', re.compile(r'(?m)(?:^|[ \t])' + SENSITIVE + r'\s*=\s*[\"\']([^\"\'\n]+)[\"\']')),
    ('sql-password', re.compile(r'\bPASSWORD\s+\'([^\'\n]+)\'', re.I)),
    ('password-bearing-url', re.compile(r'postgres(?:ql)?://[^\s/:@]+:([^\s@]+)@', re.I)),
    ('password-config', re.compile(r'^\s*password\s*=\s*[\"\']([^\"\']+)[\"\']', re.M | re.I)),
    ('password-default', re.compile(r'env\(\s*[\"\'](?:DB_PASS|PGPASSWORD|POSTGRES_PASSWORD)[\"\']\s*,\s*[\"\']([^\"\']+)[\"\']')),
    ('password-argument', re.compile(r'--database\.password=([^\s\\]+)')),
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
            if not dynamic(match.group(1)):
                result.append(field)
    if suffix == '.json':
        try:
            def walk(value):
                if isinstance(value, dict):
                    if re.fullmatch(SENSITIVE, str(value.get('name', ''))) and 'value' in value:
                        secret_ref = value.get('valueFrom', {}).get('secretKeyRef', {})
                        deletes_old_literal = (value['value'] is None
                                               and bool(secret_ref.get('name'))
                                               and bool(secret_ref.get('key')))
                        if not deletes_old_literal:
                            result.append(value['name'])
                    for child in value.values():
                        walk(child)
                elif isinstance(value, list):
                    for child in value:
                        walk(child)
            walk(json.loads(text))
        except ValueError:
            result.append('invalid-json')
    else:
        lines = text.splitlines()
        for i, line in enumerate(lines):
            name = ENV_NAME.search(line)
            if not name:
                continue
            # Flow mappings have the value on the same line. Block mappings
            # end before the next env item or an equally indented sibling.
            value = INLINE_VALUE.search(line)
            if value:
                result.append(name.group(1))
                continue
            if re.search(r'[\"\']?\bvalueFrom[\"\']?\s*:', line):
                continue
            indent = len(line) - len(line.lstrip())
            for following in lines[i + 1:]:
                if re.search(r'[\"\']?\bvalueFrom[\"\']?\s*:', following):
                    break
                if INLINE_VALUE.search(following):
                    result.append(name.group(1))
                    break
                if following.strip() and len(following) - len(following.lstrip()) <= indent:
                    break
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
