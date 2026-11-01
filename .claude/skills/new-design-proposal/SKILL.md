---
name: new-design-proposal
description: Start a new Ichnos design proposal in docs/proposals from the project template. Use when a change needs a design proposal, such as a change to the configuration or report formats, a new command, or support for a new CI provider.
---

# New Design Proposal

Follow the process in [docs/proposals/README.md](../../../docs/proposals/README.md).

1. Confirm there is an issue that describes the problem. If there is none, stop and ask the user to open one first.
2. Find the next free number: list `docs/proposals/` and take the highest `NNN-` prefix plus one, zero-padded to three digits.
3. Copy `docs/proposals/001-proposal-template.md` to `docs/proposals/NNN-short-title.md`, using a short kebab-case title.
4. Fill in the header: the authors (ask the user for their name and GitHub ID), today's date as **Begin Design Discussion**, and **Status**: provisional.
5. Fill in each section from what the user has told you. Leave a section's guidance text in place where you have no information, and list those sections for the user instead of inventing content.
6. Link the issue under **Background**.
