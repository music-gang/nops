<!--
The PR title becomes the commit message on main (squash merge), so write it in
Angular style: type(scope): subject, e.g. "feat(hooks): dispatch pre-hooks".
-->

## Summary

<!-- What changes and why, in plain language. After the squash this is the body of the commit on main. -->

## Checklist

- [ ] Tests added or updated (unit; integration if it touches `engine`, `hooks` or `nomadx`)
- [ ] Docs updated (`docs/`, `examples/`, README) if behaviour, HCL syntax or states change
- [ ] Row added to `docs/design/decisions.md` if a design decision was taken
- [ ] The invariants in `docs/philosophy.md` still hold (plan before write, CAS on register, no auto-apply under `approval`, ...)
