# Design Proposals

Larger changes to Ichnos start as a design proposal in this directory. Proposals record what was decided, why, and which alternatives were considered.

A proposal is needed for:

* Changes to the configuration or report formats
* New commands, or changes to existing command behavior
* Support for a new CI provider or builder
* Changes to how cache misses are explained or how confidence is assigned
* Anything that changes permissions, tokens or the handling of untrusted input

Bug fixes, new audit rules and documentation changes do not need a proposal.

## Process

1. Open an issue that describes the problem, and agree that it is worth solving.
2. Copy [001-proposal-template.md](001-proposal-template.md) to `NNN-short-title.md` in this directory, using the next free number, and open a pull request.
3. The maintainers accept or reject the proposal. The outcome is recorded in the proposal's status, and the proposal is merged whatever the outcome.
4. Implementation pull requests link to the proposal, and its status is updated when the work ships.
