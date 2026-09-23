## What this changes

<!-- One paragraph. Name the AC or spec section when there is one. -->

## Which AC / spec section

<!-- e.g. SPEC-01 §2.3 AC-4, or "amends SPEC-10 §3.2a". If no AC covers this,
     say which spec was amended (letter suffix) and that selfcheck passes. -->

## Verification — paste the real output

<!-- The commands you ran and their actual output. A gate claimed green without
     the run counts as unverified. -->

```text
python3 specs/tools/selfcheck.py
make check
```

## Checklist

- [ ] The spec set still passes `python3 specs/tools/selfcheck.py`
- [ ] `make check` passes (or the failure is a documented host-measured fence
      verdict, quoted above)
- [ ] `make smoke-e2e` passes if the CLI, config resolution or the checker changed
- [ ] `make schema-check` passes if a registry descriptor changed
- [ ] No secrets, tokens, private hostnames, home paths or personal data in the
      diff, tests, fixtures or docs
- [ ] Tests name the AC or spec section they verify
- [ ] Commit author is the project identity (no co-author trailers)
- [ ] Docs/CHANGELOG updated when the change is user-visible
