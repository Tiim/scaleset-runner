# Use Case: Add Repository

## Overview

**Use Case ID:** UC-002  
**Use Case Name:** Add Repository  
**Primary Actor:** Operator  
**Secondary Actors:** GitHub  
**Goal:** Have the Jobs of one more repository run on the Host through its own Scale Set, without a code change.  
**Trigger:** Operator decides to serve a repository's Jobs on the Host  
**Status:** Draft  

**Requirements:** [FR-001, FR-002, FR-004, FR-005, FR-016, FR-019, FR-020, FR-027, FR-036, C-006, C-009](../requirements.md)

## Preconditions

- The repository belongs to the Operator's personal GitHub account.
- The Operator can change the repository's settings.
- The Controller's service definition with its Config exists on the Host.

## Main Success Scenario

1. Operator checks the repository's fork pull request settings against the README.
2. Operator adds the repository to the Config, optionally with its own Scale Set name and Runner Image.
3. Operator redeploys the Controller (UC-001 Deploy Controller).
4. System validates the Config, including the new entry.
5. System creates the Scale Set for the repository under its configured name with its Instance label and records it in the State Store.
6. System polls the new Scale Set alongside the existing ones and advertises the Slot limit as its Capacity.
7. Operator sees the new Scale Set in the repository's runner settings and the Controller reported healthy; Workflow Authors can target it by its name.

## Alternative Flows

### A1: Public Repository Not Secured

**Trigger:** The repository is public and does not meet the fork pull request settings the README describes (step 1)  
**Flow:**

1. Operator requires approval for all external contributors and removes every workflow that runs fork pull request code with repository privileges.
2. Use case continues at step 2.

### A2: Operator Does Not Secure the Repository

**Trigger:** The Operator cannot or does not want to change the settings of a public repository (step 1)  
**Flow:**

1. Operator leaves the repository out of the Config.
2. Use case ends.

### A3: Invalid Config Entry

**Trigger:** The new entry is invalid, e.g. the repository is listed twice or its name is malformed (step 4)  
**Flow:**

1. System names the invalid entry in its log and exits with an error.
2. Operator corrects the entry.
3. Use case continues at step 3.

### A4: Name Already Taken

**Trigger:** The repository already has a Scale Set with the configured name that the State Store does not record and that does not carry the Controller's Instance label (step 5)  
**Flow:**

1. System leaves the existing Scale Set untouched and logs the name clash naming the repository.
2. System serves the other repositories and reports unhealthy.
3. Operator chooses another Scale Set name or removes the existing Scale Set on GitHub.
4. Use case continues at step 3.

### A5: Credentials Do Not Cover the Repository

**Trigger:** GitHub refuses to create the Scale Set because the credentials have no access to the repository (step 5)  
**Flow:**

1. System logs the refusal naming the repository, serves the other repositories, reports unhealthy, and retries with growing delays.
2. Operator grants the configured GitHub credentials access to the repository.
3. Use case continues at step 5.

## Postconditions

### Success Postconditions

- The repository has exactly one Scale Set under its configured name, recorded in the State Store and polling.
- The new Scale Set shares the Controller's Slots with the existing Scale Sets.
- The Scale Sets of the other repositories are unchanged.

### Failure Postconditions

- No Scale Set that the State Store does not record and that does not carry the Controller's Instance label is changed or deleted.
- No Scale Set of another repository is deleted.
- The Controller does not serve a public repository whose entry the Operator left out of the Config.

## Business Rules

### BR-001: Takes Effect on Redeploy

A change to the Config takes effect only when the Controller is redeployed.

### BR-002: Scale Set Name

The Scale Set takes the name set for the repository in the Config; without one, it takes the global name from the Config.

### BR-003: Public Repository Settings

A public repository is added only when its settings and workflows meet constraint C-009.

### BR-004: Personal Account Scope

Only repositories of the personal GitHub account are added; organization repositories are out of scope.

### BR-005: Per-Repository Runner Image

A per-repository Runner Image must be based on the official actions-runner image and must itself contain the Docker daemon and compose plugin when its Jobs need them.
