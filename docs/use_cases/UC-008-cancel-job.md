# Use Case: Cancel Job

## Overview

**Use Case ID:** UC-008  
**Use Case Name:** Cancel Job  
**Primary Actor:** Workflow Author  
**Secondary Actors:** GitHub, Docker Engine  
**Goal:** Cancel a Job on the Host so that it frees its Slot and leaves no Runner Container or Runner Registration behind, without disturbing other Jobs.  
**Trigger:** Workflow Author cancels a workflow run with a Job assigned to a Scale Set  
**Status:** Draft  

**Requirements:** [FR-007, FR-010, FR-028, FR-029, NFR-012](../requirements.md)

## Preconditions

- The Job is assigned to a Scale Set of the Controller.

## Main Success Scenario

1. Workflow Author cancels the workflow run.
2. GitHub withdraws the Job before any runner has started it and reports a lower assigned Job count for the Scale Set in the next poll.
3. System notices that the Scale Set has more Runner Containers than assigned Jobs.
4. System picks an Idle Runner Container of the Scale Set and deletes its Runner Registration on GitHub.
5. GitHub confirms the deletion, and System removes the Runner Container.
6. System frees the Slot.
7. Workflow Author sees the Job cancelled in the workflow run.

## Alternative Flows

### A1: Job Already Running

**Trigger:** The Job has already started in a Runner Container (step 2)  
**Flow:**

1. GitHub tells the runner to cancel the Job; the Job stops and the runner exits.
2. System removes the Runner Container as for a completed Job (UC-007 Run Job on Host).
3. Use case continues at step 7.

### A2: Only a Start Request Waits

**Trigger:** The Scale Set has a waiting start request instead of a surplus Runner Container (step 3)  
**Flow:**

1. System drops the start request without starting a Runner Container.
2. Use case continues at step 7.

### A3: Idle Runner Container Holds a Job

**Trigger:** GitHub refuses the deletion because the runner already holds a Job whose JobStarted message has not arrived yet (step 4)  
**Flow:**

1. System keeps the Runner Container as running a Job and logs that it did, naming the repository and Runner Container.
2. If the Scale Set still has more Runner Containers than assigned Jobs and another Idle Runner Container, use case continues at step 4 with that one.
3. Otherwise use case continues at step 7; the surplus is resolved when the next poll reports the assigned Job count.

### A4: Job Already Completed

**Trigger:** The Job completes before GitHub withdraws it (step 2)  
**Flow:**

1. System removes the Runner Container as for a completed Job (UC-007 Run Job on Host).
2. Use case ends.

### A5: Runner Registration Cannot Be Deleted

**Trigger:** GitHub does not answer the deletion of the Runner Registration, or refuses it for a reason other than a Job the runner holds (step 4)  
**Flow:**

1. System logs the failure, keeps the Runner Container and its Runner Registration, and retries the deletion with growing delays while the Scale Set has more Runner Containers than assigned Jobs.
2. Use case continues at step 5 once GitHub confirms the deletion, or at A3 once GitHub refuses it because the runner holds a Job.

### A6: Runner Container Cannot Be Removed

**Trigger:** The Docker Engine refuses or does not answer the removal of the Runner Container (step 5)  
**Flow:**

1. System logs the failure naming the repository and Runner Container.
2. System keeps the Runner Container recorded in the State Store and retries the removal with growing delays; until it is removed, the Runner Container keeps its Slot.
3. Use case continues at step 6 once the removal succeeds.

## Postconditions

### Success Postconditions

- No Runner Container or Runner Registration of the cancelled Job remains.
- The Slot is free again.

### Failure Postconditions

- A Runner Container that holds a Job is never removed because of the cancellation of another Job.
- No more Runner Containers run than the Slot limit allows.
- Every Runner Registration that remains is recorded in the State Store for later deletion.

## Business Rules

### BR-001: Idle Runner Removal Time

A Runner Container whose Job was cancelled before it started is removed, with its Runner Registration, within 30 seconds of the Scale Set's assigned Job count dropping.

### BR-002: GitHub Decides Whether a Runner Is Idle

When a Scale Set has more Runner Containers than assigned Jobs, System removes only Idle Runner Containers, and only after GitHub has confirmed the deletion of their Runner Registration. A JobStarted message can arrive long after GitHub has handed the runner a Job, so the absence of one does not prove a Runner Container idle.

### BR-003: Start Request Order Kept

Dropping a waiting start request does not change the order of the other start requests.
