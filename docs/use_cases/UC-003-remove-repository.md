# Use Case: Remove Repository

## Overview

**Use Case ID:** UC-003  
**Use Case Name:** Remove Repository  
**Primary Actor:** Operator  
**Secondary Actors:** GitHub  
**Goal:** Stop serving a repository's Jobs and leave no stale Scale Set on GitHub that keeps accepting Jobs nobody serves.  
**Trigger:** Operator decides to stop serving a repository's Jobs on the Host  
**Status:** Draft  

**Requirements:** [FR-003, FR-029, FR-031, C-007](../requirements.md)

## Preconditions

- The repository's Scale Set is recorded in the Controller's State Store.

## Main Success Scenario

1. Operator removes the repository from the Config.
2. Operator redeploys the Controller (UC-001 Deploy Controller).
3. System finds every Scale Set recorded in the State Store whose repository or name is no longer in the Config.
4. System deletes each of these Scale Sets on GitHub.
5. System removes their records from the State Store.
6. Operator sees that the repository's runner settings no longer list the Scale Set.

## Alternative Flows

### A1: Scale Set Renamed

**Trigger:** Operator changes the Scale Set name of a repository instead of removing it (step 1)  
**Flow:**

1. System treats the old name as removed and the new name as added (UC-002 Add Repository).
2. Use case continues at step 2.

### A2: Scale Set Already Gone

**Trigger:** GitHub reports that the Scale Set no longer exists (step 4)  
**Flow:**

1. System treats the Scale Set as deleted.
2. Use case continues at step 5.

### A3: Deletion Fails

**Trigger:** GitHub refuses or does not answer the deletion (step 4)  
**Flow:**

1. System logs the failure naming the repository and Scale Set.
2. System keeps the record in the State Store and retries the deletion with growing delays while it serves the other repositories; once a retry succeeds, System removes the record.
3. Use case ends.

## Postconditions

### Success Postconditions

- The repository has no Scale Set of this Controller on GitHub and no record in the State Store.
- No Runner Container or Runner Registration of the removed Scale Set remains.
- The Scale Sets of the other repositories are unchanged.

### Failure Postconditions

- A Scale Set that still exists on GitHub keeps its record in the State Store, so a later start deletes it.
- No Scale Set that the State Store does not record is deleted.
- No Scale Set whose repository and name are still in the Config is deleted.

## Business Rules

### BR-001: Recorded Scale Sets Only

The Controller deletes only Scale Sets recorded in its own State Store; Scale Sets of other Controllers or created by hand stay untouched.

### BR-002: Rename Is Remove Plus Add

A Scale Set whose name changes in the Config counts as removed under its old name and added under its new name.

### BR-003: Takes Effect at Startup

Stale Scale Sets are found only when the Controller starts; a change to the Config while it runs deletes nothing. A deletion that fails at startup is retried while the Controller runs.

### BR-004: Waiting Jobs

Jobs that still wait for the deleted Scale Set are no longer served by the Controller.
