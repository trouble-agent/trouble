#!/usr/bin/env python3
"""Resolve docs/operations.md conflict: BOTH section-12s are wanted — dashboard (SPEC-10)
landed as §12 on main; lifecycle (SPEC-12) numbered itself §12 on its branch.
Resolution: keep both, renumber lifecycle to §13, verify no duplicate headings."""
import re

p = 'docs/operations.md'
text = open(p).read()

m = re.search(r'<<<<<<< HEAD\n(.*?)\n=======\n(.*?)\n>>>>>>> codex/lifecycle-spec12\n', text, re.S)
assert m, "conflict markers not found"
head_side, branch_side = m.group(1), m.group(2)

# lifecycle side: renumber its internal cross-refs 12.x -> 13.x where they refer to its own sections
branch_fixed = branch_side.replace('§12', '§13').replace('## 12.', '## 13.')
# keep its top heading as ## 13.
branch_fixed = re.sub(r'^## 12\.', '## 13.', branch_fixed, count=1, flags=re.M)

resolved = head_side + "\n\n" + branch_fixed + "\n"
text = text[:m.start()] + resolved + text[m.end():]
open(p, 'w').write(text)

h12 = len(re.findall(r'^## 12\.', text, re.M))
h13 = len(re.findall(r'^## 13\.', text, re.M))
leftover = '<<<<<<<' in text or '>>>>>>>' in text
print(f"§12 headings: {h12}  §13 headings: {h13}  leftover markers: {leftover}")
