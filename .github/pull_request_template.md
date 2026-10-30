## Problem

Describe what went wrong or what need emerged to justify this bug fix, feature, or change.
Link any relevant issues here.

## Solution

Provide a brief description of the change and the key choices you made when architecting the solution.

## Verification

Provide the commands run to verify this change:

1. `make vet`
2. `make race`
3. `make fmt`

## Checklist

Check all before review:

- [ ] I have read [CONTRIBUTING.md](.github/CONTRIBUTING.md)
- [ ] All tests pass under the race detector (`make race`) and static analysis (`make vet`)
- [ ] If HTTP endpoints or payloads changed, I updated [api/openapi.yaml](api/openapi.yaml) and [docs/USAGE.md](docs/USAGE.md)
