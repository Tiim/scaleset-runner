# Use Case: Shut Down Controller

## Overview

**Use Case ID:** UC-004  
**Use Case Name:** Shut Down Controller  
**Primary Actor:** Operator  
**Secondary Actors:** GitHub, Docker Engine  
**Goal:** Stop the Controller so that running Jobs can finish and no Runner Container or Runner Registration is left behind.  
**Trigger:** Operator stops or redeploys the Controller  
**Status:** Draft  

**Requirements:** [FR-007, FR-015, FR-021, FR-028, FR-029, FR-031, NFR-007, NFR-008, C-004](../requirements.md)

## Preconditions

- The Controller is running.

## Main Success Scenario

1. Operator asks the Controller to stop.
2. System stops taking Jobs on every Scale Set.
3. System drops its waiting start requests without starting Runner Containers for them.
4. For every Idle Runner Container, System deletes its Runner Registration and, once GitHub confirms the deletion, removes the Runner Container.
5. System waits for the running Jobs to finish, up to the shutdown timeout.
6. System removes each Runner Container as soon as its Job finishes, together with its Runner Registration.
7. System exits, and the Operator sees the Controller stopped with none of its Runner Containers left.

## Alternative Flows

### A1: Idle Runner Registration Cannot Be Deleted

**Trigger:** GitHub does not answer the deletion of the Runner Registration of an Idle Runner Container (step 4)  
**Flow:**

1. System logs the failure, keeps the Runner Container, and retries the deletion with growing delays until the shutdown timeout expires.
2. Use case continues at step 5.

### A2: No Running Jobs

**Trigger:** No Job is running when the Controller stops taking Jobs (step 5)  
**Flow:**

1. Use case continues at step 7.

### A3: Shutdown Timeout Expires

**Trigger:** Jobs are still running when the shutdown timeout expires (step 5)  
**Flow:**

1. System removes the remaining Runner Containers together with their Runner Registrations; their Jobs end without success. A Runner Registration that cannot be deleted stays recorded in the State Store, so the next start deletes it.
2. System logs every Job it ended, naming the repository, Job ID, and Runner Container.
3. Use case continues at step 7.

### A4: Runner Registration Cannot Be Deleted

**Trigger:** GitHub refuses or does not answer the deletion of a Runner Registration (step 6)  
**Flow:**

1. System logs the failure and keeps the Runner Registration recorded in the State Store, so the next start deletes it.
2. Use case continues at step 7.

### A5: Controller Killed

**Trigger:** The Host kills the Controller before it finishes (step 5)  
**Flow:**

1. The remaining Runner Containers and Runner Registrations stay recorded in the State Store.
2. The next start removes them (UC-001 Deploy Controller).
3. Use case ends.

### A6: Idle Runner Container Holds a Job

**Trigger:** GitHub refuses the deletion because the runner already holds a Job whose JobStarted message has not arrived yet (step 4)  
**Flow:**

1. System keeps the Runner Container as running a Job.
2. Use case continues at step 5.

### A7: Runner Container Cannot Be Removed

**Trigger:** The Docker Engine refuses or does not answer the removal of a Runner Container (step 4 or 6)  
**Flow:**

1. System logs the failure naming the Runner Container.
2. System retries the removal with growing delays until the shutdown timeout expires; a Runner Container that remains stays recorded in the State Store, so the next start removes it.
3. Use case continues at step 7.

## Postconditions

### Success Postconditions

- No Runner Container carrying the Controller's Instance ID remains on the Host.
- No Runner Registration of the Controller remains on GitHub.
- Every Scale Set stays registered on GitHub and recorded in the State Store, so Jobs wait for the next start.

### Failure Postconditions

- Every Runner Container or Runner Registration that remains is recorded in the State Store, so the next start removes it.
- No Scale Set is deleted.
- No Runner Container carrying another Instance ID is touched.

## Business Rules

### BR-001: Shutdown Timeout

Running Jobs may finish for up to the shutdown timeout, 5 minutes by default and configurable.

### BR-002: Grace Period

The Host's grace period for stopping the Controller exceeds the shutdown timeout.

### BR-003: Fast Exit Without Jobs

With no running Jobs, the Controller exits and has removed its Runner Containers within 30 seconds of the stop request.

### BR-004: Scale Sets Persist

Shutting down never deletes a Scale Set; only removing its repository from the Config does (UC-003 Remove Repository).

### BR-005: GitHub Decides Whether a Runner Is Idle

An Idle Runner Container is removed only after GitHub has confirmed the deletion of its Runner Registration (UC-008 BR-002).
