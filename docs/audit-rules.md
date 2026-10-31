# Cache Setup Audit Rules

> [!IMPORTANT]
> Ichnos is not released yet. These rules are planned, and the list will grow.

Each audit rule has a permanent ID, a documentation page and a suggested fix. A rule fires only on evidence from the build record, the CI configuration, the Dockerfile or the CI provider's API. Rule IDs are never reused or renamed.

| Rule | Fires when | Suggested fix |
|---|---|---|
| `no-cache-import` | The pull request build imported no cache, although the base build did | Add `cache-from` for both the branch and the default branch |
| `readonly-trigger-export` | `cache-to` runs on a trigger that can only read the GitHub Actions cache, such as `pull_request_target` | Keep only `cache-from` on those triggers |
| `min-mode-multistage` | A multi-stage Dockerfile exports its cache without `mode=max`, so earlier stages are never cached | Export with `mode=max` |
| `shared-cache-target` | Several images write the same cache scope and overwrite each other | Give each image its own `scope=` |
| `git-dir-in-context` | `COPY .` copies `.git` because `.dockerignore` does not exclude it | Add `.git` to `.dockerignore` |

To propose a new rule, see [Adding an Audit Rule](../CONTRIBUTING.md#adding-an-audit-rule).
