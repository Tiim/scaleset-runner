# Use Case: Run Job on Host

## Overview

**Use Case ID:** UC-007  
**Use Case Name:** Run Job on Host  
**Primary Actor:** Workflow Author  
**Secondary Actors:** GitHub, Docker Engine  
**Goal:** Have a Job run on the Host in a fresh, isolated Runner Container that leaves nothing behind.  
**Trigger:** A workflow run in a configured repository starts a Job that targets a Scale Set by its name  
**Status:** Draft  

**Requirements:** [FR-004, FR-006, FR-007, FR-008, FR-009, FR-010, FR-011, FR-012, FR-013, FR-021, FR-024, FR-025, FR-029, FR-032, FR-033, FR-034, FR-035, NFR-002, NFR-003, NFR-006, NFR-007, NFR-013, C-005](../requirements.md)

## Preconditions

- The repository's Scale Set is registered and the Controller polls it.
- The Runner Image of the repository is present on the Host.

## Main Success Scenario

1. Workflow Author triggers a workflow whose Job targets the Scale Set by its name.
2. GitHub offers the Job to the Scale Set in the next poll.
3. System acquires the Job, and GitHub raises the Scale Set's assigned Job count.
4. System sees that the Scale Set's assigned Job count exceeds its Runner Containers and makes a start request.
5. System takes a free Slot for the start request and updates the Capacity it advertises for every Scale Set.
6. System obtains a JIT Config for a new Runner Registration from GitHub.
7. System starts a Runner Container from the repository's Runner Image with the JIT Config, its own entrypoint, and the configured CPU and memory limits, isolated from the Host.
8. The Runner Container starts its own Docker daemon and then the runner, and writes the Ready marker once the runner has connected to GitHub.
9. GitHub hands one of the Scale Set's assigned Jobs to the runner, and System learns the Job from the JobStarted message.
10. The Job runs, including any image builds and compose stacks it needs, and restores and saves its Go and Docker caches through the GitHub Actions cache service as the README shows.
11. The Job completes, the runner exits, and System receives the JobCompleted message.
12. System deletes the Runner Registration, removes the Runner Container, frees the Slot, and updates the Capacity it advertises for every Scale Set.
13. Workflow Author sees the result of the Job in the workflow run.

## Alternative Flows

### A1: Acquisition Fails

**Trigger:** GitHub refuses or does not answer the acquisition of the offered Job (step 3)  
**Flow:**

1. System logs the failure naming the repository and leaves the poll unacknowledged.
2. System polls again with growing delays, and GitHub offers the Job again.
3. Use case continues at step 3.

### A2: No Free Slot

**Trigger:** All Slots are busy when the start request arises (step 5)  
**Flow:**

1. The start request waits.
2. When a Slot frees up, System takes it for the start request that arose earliest across all Scale Sets.
3. Use case continues at step 6.

### A3: JIT Config Refused

**Trigger:** GitHub refuses or does not answer the request for a JIT Config (step 6)  
**Flow:**

1. System logs the failure naming the repository.
2. System retries with growing delays while the Scale Set's assigned Job count still exceeds its Runner Containers.
3. Use case continues at step 6.

### A4: Demand Drops During a Retry

**Trigger:** The Scale Set's assigned Job count no longer exceeds its Runner Containers while System retries the JIT Config or the start of the Runner Container, e.g. because the Job was cancelled (step 6 or 7)  
**Flow:**

1. System drops the start request, stops retrying, deletes the Runner Registration, if any, removes the Runner Container, if any, frees the Slot, and updates the Capacity it advertises.
2. System logs that the start request was dropped, naming the repository.
3. Use case ends.

### A5: Runner Container Cannot Start

**Trigger:** The Runner Container cannot be started, e.g. its Runner Image is unusable or the Docker Engine refuses (step 7)  
**Flow:**

1. System deletes the Runner Registration, removes the Runner Container, if any, and frees the Slot.
2. System logs the failure naming the repository and Runner Container.
3. System retries with growing delays and a new JIT Config while the Scale Set's assigned Job count still exceeds its Runner Containers.
4. Use case continues at step 5.

### A6: No Ready Marker

**Trigger:** The Runner Container has not written the Ready marker within the readiness timeout, e.g. because its Docker daemon or the runner failed to start (step 8)  
**Flow:**

1. System deletes the Runner Registration.
2. If GitHub refuses because the runner already holds a Job, System keeps the Runner Container as running that Job, logs the missing Ready marker as a warning, and the use case continues at step 9.
3. Otherwise System removes the Runner Container, frees the Slot, and logs the failure naming the repository and Runner Container.
4. System retries with growing delays and a new JIT Config while the Scale Set's assigned Job count still exceeds its Runner Containers.
5. Use case continues at step 5.

### A7: Job Cancelled

**Trigger:** Workflow Author cancels the workflow run before the Job completes (step 4 to 10)  
**Flow:**

1. System handles the cancellation (UC-008 Cancel Job).
2. Use case ends.

### A8: Job Exceeds Its Limits

**Trigger:** The Job uses more memory than the Runner Container's limit (step 10)  
**Flow:**

1. The Job fails.
2. Use case continues at step 11.

### A9: Runner Container Exits Without JobCompleted

**Trigger:** The runner or its Runner Container stops without a JobCompleted message, with or without a Job (step 8 to 10)  
**Flow:**

1. System logs the stop naming the repository, Runner Container, and, if known, the Job.
2. System deletes the Runner Registration, removes the Runner Container, and frees the Slot.
3. System makes no new start request for the Scale Set until a later poll has reported its assigned Job count; GitHub concludes a Job the runner held as failed and lowers the count.
4. Use case continues at step 13.

### A10: Runner Registration Cannot Be Deleted

**Trigger:** GitHub refuses or does not answer the deletion of the Runner Registration (step 12)  
**Flow:**

1. System logs the failure and keeps the Runner Registration recorded in the State Store, so the next start deletes it.
2. System still removes the Runner Container, frees the Slot, and updates the Capacity it advertises.
3. Use case continues at step 13.

### A11: Runner Container Cannot Be Removed

**Trigger:** The Docker Engine refuses or does not answer the removal of the Runner Container (step 12)  
**Flow:**

1. System logs the failure naming the repository and Runner Container.
2. System keeps the Runner Container recorded in the State Store and retries the removal with growing delays; until it is removed, the Runner Container keeps its Slot.
3. Once the removal succeeds, System frees the Slot and updates the Capacity it advertises for every Scale Set.
4. Use case continues at step 13.

## Postconditions

### Success Postconditions

- The Job has concluded and its result is visible in the workflow run.
- The Runner Container and its Runner Registration are removed.
- The Slot is free again and every Scale Set advertises its updated Capacity.
- The logs trace the Job from the start of its Runner Container to its removal.

### Failure Postconditions

- No more Runner Containers run than the Slot limit allows.
- No Slot stays taken by a start request once the Scale Set's assigned Job count no longer exceeds its Runner Containers.
- No Runner Container or Runner Registration remains once it no longer serves an assigned Job, except a Runner Registration recorded in the State Store for removal at the next start.
- No JIT Config appears in the logs.

## Business Rules

### BR-001: One Runner Container per Job

Every Job runs in its own freshly created Runner Container, which executes exactly one Job, chosen by GitHub among the Scale Set's assigned Jobs, and is then removed.

### BR-002: Shared Slot Limit

The number of running Runner Containers never exceeds the Controller's Slot limit across all its Scale Sets.

### BR-003: Capacity

Every Scale Set advertises its own Runner Containers plus the free Slots as its Capacity. GitHub reads it with the next poll, so a poll already running may still lead to Jobs beyond the free Slots; their start requests wait (A2).

### BR-004: Start Request Order

Start requests are served in the order they arose across all Scale Sets.

### BR-005: Resource Limits

Every Runner Container gets the same CPU and memory limit, 2 CPUs and 4 GiB by default.

### BR-006: Isolation

A Runner Container runs isolated by the sysbox runtime, without elevated privileges, without access to the Host's Docker daemon, and without storage shared with other containers.

### BR-007: Start Latency

With a free Slot and the Runner Image present, the Runner Container writes the Ready marker within 15 seconds of the start request arising.

### BR-008: Acquire Every Offered Job

System acquires every Job GitHub offers a Scale Set, even while no Slot is free.

### BR-009: Readiness Timeout

A Runner Container that has not written the Ready marker within the readiness timeout, 120 seconds by default and configurable, has failed to start unless GitHub reports that its runner holds a Job.

### BR-010: Replace From Fresh Demand

After a Runner Container exits without a JobCompleted message, System makes a new start request for its Scale Set only from an assigned Job count reported by a later poll.

### BR-011: Job ID in Logs

The logs name the Job of a Runner Container from its JobStarted message on; before that, they name the repository and Runner Container only.
