# Branch protection

`main` is protected by the repository ruleset `protect-main`.

- **Changes go through a pull request.** Direct pushes to `main` are blocked.
  Required approvals: 0, since this is a solo project.
- **Required status checks:** `test` (build and `go test -race`) and `lint`
  (`golangci-lint`). The branch must be up to date with `main` before merging.
- **Force-pushes and branch deletion are blocked.**

The checks are defined in `.github/workflows/ci.yml`. If a CI job is renamed,
update the required check names under Settings → Rules → Rulesets.
