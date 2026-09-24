#!/usr/bin/env python3
"""Build specs-review.html — every spec rendered, with a linked TOC and per-file anchors.
Order: INDEX first, then SPEC-01..12, TYPES last. Dark theme, code blocks styled,
spec-to-spec links resolved to anchors.

Usage:  python3 docs/build_specs_review.py
Run from anywhere; the repository root is derived from this file's location, so the
script carries no host path of its own and no checkout location is written into the
generated HTML.
"""
import glob
import html as H
import os
import re
import subprocess

import markdown

DOCS = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(DOCS)
SPECS = os.path.join(REPO, 'specs')

ORDER = [os.path.join(SPECS, 'SPEC-INDEX.md')] + \
        sorted(glob.glob(os.path.join(SPECS, 'SPEC-0*.md'))) + \
        [os.path.join(SPECS, 'SPEC-TYPES.md')] + \
        sorted(glob.glob(os.path.join(SPECS, 'SPEC-1*.md')))


def repo_state() -> str:
    """How the renderer's own checkout looked when this page was generated.

    Deliberately NOT "the HEAD revision": a generated page cannot name the
    commit that contains it, so a revision string here is always one revision
    stale and would name a tree whose specs differ from the ones rendered.
    Report the revision together with whether the tree was clean instead.
    """
    try:
        head = subprocess.run(['git', '-C', REPO, 'rev-parse', '--short', 'HEAD'],
                              capture_output=True, text=True, check=False)
        dirty = subprocess.run(['git', '-C', REPO, 'status', '--porcelain'],
                               capture_output=True, text=True, check=False)
        rev = head.stdout.strip() or 'unknown'
        return f"{rev}{' (working tree with local edits)' if dirty.stdout.strip() else ''}"
    except OSError:
        return 'unknown'


md = markdown.Markdown(extensions=['tables', 'fenced_code', 'toc', 'codehilite'],
                       extension_configs={'codehilite': {'guess_lang': False}})


def slug(p: str) -> str:
    return os.path.basename(p).replace('.md', '').lower().replace('_', '-')


# pre-scan: first heading of each file for the TOC
toc_entries: list[tuple[str, str, str]] = []
for p in ORDER:
    with open(p, encoding='utf-8') as fh:
        txt = fh.read()
    m = re.search(r'^#\s+(.+)$', txt, re.M)
    toc_entries.append((slug(p), os.path.basename(p), m.group(1) if m else os.path.basename(p)))

sections: list[str] = []
for p in ORDER:
    with open(p, encoding='utf-8') as fh:
        txt = fh.read()
    md.reset()
    body = md.convert(txt)
    # resolve relative links to other spec files -> anchors within this page
    body = re.sub(r'href="((?:\.\./)?specs/)?(SPEC-[A-Z0-9-]+\.md)(#[^"]*)?"',
                  lambda m: f'href="#{m.group(2).lower().replace("_", "-")}"', body)
    s = slug(p)
    sections.append(f'<section id="{s}"><div class="filetag">{os.path.basename(p)}</div>\n{body}\n</section>')

toc_html = '\n'.join(
    f'<a class="tocitem" href="#{s}"><code>{fn}</code> — {H.escape(title[:80])}</a>'
    for s, fn, title in toc_entries)

css = """
* { margin:0; padding:0; box-sizing:border-box; }
body { background:#0a0a14; color:#c9cede; font-family:-apple-system,'Segoe UI',Roboto,sans-serif; line-height:1.62; padding:14px; -webkit-text-size-adjust:100%; }
.wrap { max-width:980px; margin:0 auto; }
header { padding:26px 6px 18px; border-bottom:2px solid #f8717133; margin-bottom:16px; }
h1 { font-size:1.7rem; color:#f87171; } .sub { color:#7e8aa8; font-size:.9rem; margin-top:6px; }
.toc { background:#10101e; border:1px solid #26263c; border-radius:12px; padding:14px 16px; margin-bottom:20px; }
.toc h2 { font-size:.8rem; color:#8b9ab8; text-transform:uppercase; letter-spacing:.1em; margin-bottom:10px; }
.tocitem { display:block; color:#9fc3e8; text-decoration:none; padding:3px 0; font-size:.86rem; }
.tocitem code { color:#f0b4ab; }
.tocitem:hover { color:#fff; }
.filetag { font-size:.7rem; letter-spacing:.12em; text-transform:uppercase; color:#f87171; background:#1a1218; display:inline-block; padding:3px 10px; border-radius:6px; margin-bottom:10px; }
section { background:#0e0e1a; border:1px solid #20203a; border-radius:14px; padding:20px 18px; margin:18px 0; }
h1 { font-size:1.35rem; color:#eef0f8; margin-bottom:12px; }
section h1 { font-size:1.3rem; border-bottom:1px solid #26263c; padding-bottom:8px; }
h2 { font-size:1.1rem; color:#e8b4ab; margin:20px 0 8px; }
h3 { font-size:.98rem; color:#b8c6e0; margin:14px 0 6px; }
p { margin-bottom:10px; }
table { width:100%; border-collapse:collapse; font-size:.83rem; margin:10px 0; display:block; overflow-x:auto; }
th { text-align:left; color:#8b9ab8; font-size:.68rem; text-transform:uppercase; padding:6px 8px; border-bottom:1px solid #2a2a44; }
td { padding:7px 8px; border-bottom:1px solid #1a1a30; vertical-align:top; }
code { background:#1c1c30; border-radius:4px; padding:1px 5px; font-family:ui-monospace,Menlo,monospace; font-size:.84em; color:#e8a0a0; }
pre { background:#08080f; border:1px solid #26263c; border-radius:10px; padding:12px; overflow-x:auto; font-size:.74rem; line-height:1.5; margin:10px 0; }
pre code { background:none; padding:0; color:#a8d8b8; }
ul, ol { margin:6px 0 10px 20px; } li { margin-bottom:4px; }
blockquote { border-left:3px solid #f87171; background:#140f16; padding:10px 14px; margin:10px 0; border-radius:0 10px 10px 0; }
hr { border:none; border-top:1px dashed #26263c; margin:16px 0; }
a { color:#60a5fa; }
.footer { text-align:center; color:#4a5068; font-size:.75rem; padding:20px; }
@media print { body { background:#fff; color:#111; } section { border:none; page-break-after:always; } }
"""

page = f"""<!DOCTYPE html>
<html lang="en"><head><meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>trouble — full spec suite (review copy)</title>
<style>{css}</style></head>
<body><div class="wrap">
<header>
<h1>trouble — full spec suite</h1>
<div class="sub">SPEC-INDEX + SPEC-01..12 + SPEC-TYPES · the complete implementation contract for v0.1 · self-check PASS · built from the 3× BUILD-WITH-CHANGES quorum<br>
Review order suggestion: SPEC-INDEX (scope + cut line) → any SPEC-n (each is self-contained §1–8) → SPEC-TYPES (the shared type system).</div>
</header>
<div class="toc"><h2>Contents ({len(ORDER)} files)</h2>{toc_html}</div>
{''.join(sections)}
<div class="footer">generated from the spec suite in this repository · {repo_state()} · print-friendly: each section page-breaks</div>
</div></body></html>"""

out = os.path.join(DOCS, 'specs-review.html')
with open(out, 'w', encoding='utf-8') as fh:
    fh.write(page)
print(f"wrote {out} ({os.path.getsize(out)//1024} KB, {len(ORDER)} specs)")
