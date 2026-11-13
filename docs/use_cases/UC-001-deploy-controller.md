# Use Case: Deploy Controller

## Overview

**Use Case ID:** UC-001  
**Use Case Name:** Deploy Controller  
**Primary Actor:** Operator  
**Secondary Actors:** GitHub, Docker Engine  
**Goal:** Bring the Controller up on the Host so that every repository in the Config has a polling Scale Set and no leftovers of a previous run remain.  
**Trigger:** Operator deploys or redeploys the Controller on the Host  
**Status:** Draft  

**Requirements:** [FR-001, FR-003, FR-005, FR-008, FR-009, FR-014, FR-017, FR-018, FR-019, FR-020, FR-021, FR-022, FR-029, FR-030, FR-031, FR-036, FR-038, NFR-001, NFR-004, NFR-005, NFR-007, NFR-010, NFR-013, C-002, C-003, C-004, C-006, C-007, C-008](../requirements.md)

## Preconditions

- The Host runs Docker with the sysbox runtime installed.
- The Controller's service definition on the Host holds its Config, including its Instance ID and GitHub credentials.
- The Controller has its own State Store volume.

## Main Success Scenario

1. Operator starts the Controller with its Config.
2. System validates the Config and makes sure no other running Controller on the Host uses its Instance ID.
3. System signs in to GitHub with the configured credentials.
4. System makes sure every Runner Image the Config uses is present on the Host; for each missing one it performs UC-006 Refresh Runner Images.
5. System removes every Runner Container carrying its Instance ID and deletes every Runner Registration recorded in its State Store, whether or not its Runner Container still exists, all of them left from a previous run.
6. System deletes every Scale Set recorded in its State Store whose repository or name is no longer in the Config and removes its record, once no Runner Registration of that Scale Set is recorded any more.
7. System takes over every Scale Set under a configured name that carries its Instance label but is missing from the State Store, creates a Scale Set with its Instance label under its configured name for every configured repository that has none, records both in the State Store, and updates every Scale Set it serves to the configured settings.
8. System opens a message session for every Scale Set and advertises its Capacity: its own Runner Containers plus the free Slots.
9. System reports healthy once every Scale Set has polled successfully.
10. Operator sees the Controller reported healthy; it serves Jobs for every configured repository.

## Alternative Flows

### A1: Invalid Config

**Trigger:** The Config is missing a mandatory setting or holds an invalid value (step 2)  
**Flow:**

1. System names every invalid or missing setting in its log.
2. System exits with an error.
3. Operator corrects the Config.
4. Use case continues at step 1.

### A2: Credentials Rejected

**Trigger:** GitHub rejects the configured credentials (step 3)  
**Flow:**

1. System logs that GitHub rejected the credentials, without revealing them.
2. System exits with an error.
3. Use case ends.

### A3: GitHub Unreachable

**Trigger:** GitHub does not answer (step 3)  
**Flow:**

1. System reports unhealthy and logs the failure.
2. System retries with growing delays until GitHub answers.
3. Use case continues at step 3.

### A4: Docker Engine Unreachable

**Trigger:** The Docker Engine on the Host does not answer (step 4)  
**Flow:**

1. System logs that the Docker Engine is unreachable.
2. System exits with an error.
3. Use case ends.

### A5: Default Runner Image Cannot Be Built

**Trigger:** The default Runner Image is missing and UC-006 fails to build it (step 4)  
**Flow:**

1. System logs the build failure.
2. System exits with an error.
3. Use case ends.

### A6: Per-Repository Runner Image Cannot Be Pulled

**Trigger:** A per-repository Runner Image is missing and UC-006 fails to pull it (step 4)  
**Flow:**

1. System logs the failure naming the repository.
2. System leaves the Scale Set of that repository without a message session and retries the pull with growing delays; until it succeeds, the Scale Set has no successful poll.
3. Use case continues at step 5 for the other repositories.

### A7: Leftover Cannot Be Removed

**Trigger:** The Docker Engine refuses to remove a leftover Runner Container, or GitHub refuses or does not answer the deletion of a leftover Runner Registration (step 5)  
**Flow:**

1. System logs the failure naming the Runner Container or Runner Registration.
2. System keeps its record in the State Store and retries the removal with growing delays; until a leftover Runner Container is removed, it takes a Slot.
3. System postpones the deletion of a stale Scale Set in step 6 until the leftover Runner Registrations of that Scale Set are deleted, then deletes it.
4. Use case continues at step 6.

### A8: Stale Scale Set Cannot Be Deleted

**Trigger:** GitHub refuses or does not answer the deletion of a stale Scale Set (step 6)  
**Flow:**

1. System logs the failure naming the repository and Scale Set.
2. System keeps the Scale Set's record in the State Store and retries the deletion with growing delays.
3. Use case continues at step 7.

### A9: Foreign Scale Set with the Same Name

**Trigger:** The repository already has a Scale Set with the configured name that the State Store does not record and that does not carry the Controller's Instance label (step 7)  
**Flow:**

1. System leaves the existing Scale Set untouched.
2. System logs the name clash naming the repository and Scale Set; the repository stays unserved until the next start, so the Controller reports unhealthy.
3. Use case continues at step 8 for the other repositories.

### A10: Scale Set Cannot Be Created

**Trigger:** GitHub refuses to create the Scale Set of one repository, e.g. because the credentials do not cover it (step 7)  
**Flow:**

1. System logs the refusal naming the repository.
2. System retries the creation with growing delays while it serves the other repositories; until it succeeds, the Controller reports unhealthy.
3. Use case continues at step 8.

### A11: Message Session Cannot Be Opened

**Trigger:** GitHub refuses to open the message session of one Scale Set (step 8)  
**Flow:**

1. System logs the refusal naming the repository and Scale Set.
2. System retries with growing delays while it serves the other Scale Sets; until it succeeds, the Controller reports unhealthy.
3. Use case continues at step 9.

### A12: Instance ID in Use

**Trigger:** Another running Controller on the Host uses the same Instance ID (step 2)  
**Flow:**

1. System logs the clash naming the Instance ID, without touching any Runner Container or Scale Set.
2. System exits with an error.
3. Operator gives one of the Controllers another Instance ID.
4. Use case continues at step 1.

### A13: Scale Set Update Refused

**Trigger:** GitHub refuses or does not answer the update of a Scale Set to the configured settings (step 7)  
**Flow:**

1. System logs the failure naming the repository and Scale Set.
2. System serves the Scale Set with its current settings and retries the update with growing delays.
3. Use case continues at step 8.

## Postconditions

### Success Postconditions

- Every repository in the Config has exactly one Scale Set under its configured name, recorded in the State Store and polling.
- No Runner Container or Runner Registration from the Controller's previous run remains.
- No Scale Set is recorded in the State Store for a repository or name that is no longer in the Config.
- Every Scale Set carries the Controller's Instance label and the configured settings.
- Every Scale Set advertises its own Runner Containers plus the free Slots as its Capacity.
- The health status reports healthy.

### Failure Postconditions

- No Scale Set that the State Store does not record and that does not carry the Controller's Instance label is changed or deleted.
- No Runner Container or Scale Set carrying another Instance ID is touched.
- The Controller never reports healthy while a configured repository has no Scale Set it serves or a Scale Set has had no successful poll for longer than the health threshold.
- No credential appears in a log or error output.

## Business Rules

### BR-001: Instance Scope

The Controller removes only Runner Containers that carry its own Instance ID and deletes only Scale Sets recorded in its own State Store.

### BR-002: Slot Limit

The Slot limit defaults to 1 and is configurable from 1 to 32. It applies across all Scale Sets of one Controller.

### BR-003: Scale Set Name

A Scale Set takes the name set for its repository in the Config; without one, it takes the global name from the Config.

### BR-004: Credentials

The Config provides either a GitHub App installation or a personal access token. A Config that provides both, or neither, is invalid.

### BR-005: Health Threshold

The Controller reports unhealthy when any configured repository has no Scale Set it serves, when any Scale Set has had no successful poll for longer than the health threshold, 120 seconds by default and configurable, or when a Runner Image is overdue (UC-006 BR-003).

### BR-006: Restart Recovery

Within 60 seconds of the start, every configured Scale Set polls again and no Runner Container or Runner Registration of the previous run remains.

### BR-007: Name Ownership

The Controller takes over an existing Scale Set that its State Store does not record only when it carries the Controller's Instance label; it never changes or deletes any other Scale Set it does not record.

### BR-008: Unique Instance ID

Two running Controllers on one Host never share an Instance ID; the second one exits at startup.

### BR-009: Capacity

Every Scale Set advertises its own Runner Containers plus the free Slots as its Capacity (UC-007 BR-003).

### BR-010: Registrations Before Scale Set

A stale Scale Set is deleted only after every Runner Registration recorded for it is deleted, so that no record points to a deleted Scale Set.
