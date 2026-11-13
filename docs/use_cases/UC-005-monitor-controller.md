# Use Case: Monitor Controller

## Overview

**Use Case ID:** UC-005  
**Use Case Name:** Monitor Controller  
**Primary Actor:** Operator  
**Goal:** Find out whether the Controller serves every Scale Set, whether its Slot limit fits the load, and what happened to a given Job.  
**Trigger:** Operator wants to check the state of the Controller  
**Status:** Draft  

**Requirements:** [FR-021, FR-022, FR-026, NFR-007, C-008](../requirements.md)

## Preconditions

- The Controller is running.
- The Operator has access to the Host.

## Main Success Scenario

1. Operator requests the Controller's health status.
2. System reports healthy, because every configured repository has a Scale Set it serves, every Scale Set has polled successfully within the health threshold, and no Runner Image is overdue.
3. Operator requests the Controller's metrics.
4. System reports per Scale Set the waiting start requests, the busy Slots, and the Job durations.
5. Operator looks up a Job in the Controller's logs by repository and Job ID.
6. System's logs show every lifecycle event of the Runner Container that ran the Job, from its start to its removal, each naming the repository and Runner Container, and the Job ID from the JobStarted message on.
7. Operator concludes that the Controller works and that its Slot limit fits the load.

## Alternative Flows

### A1: Controller Unhealthy

**Trigger:** A configured repository has no Scale Set the Controller serves, a Scale Set has had no successful poll for longer than the health threshold, or a Runner Image is overdue (step 2)  
**Flow:**

1. System reports unhealthy.
2. Operator searches the logs for the unserved repository, the failing Scale Set, or the overdue Runner Image.
3. Use case continues at step 5.

### A2: Controller Does Not Answer

**Trigger:** The Controller does not answer the health request (step 2)  
**Flow:**

1. Operator redeploys the Controller (UC-001 Deploy Controller).
2. Use case ends.

### A3: Slot Limit Too Low

**Trigger:** Start requests wait while all Slots are busy (step 4)  
**Flow:**

1. Operator raises the Slot limit in the Config within what the Host can carry.
2. Operator redeploys the Controller (UC-001 Deploy Controller).
3. Use case ends.

### A4: Job Not Found

**Trigger:** The logs hold no event for the Job (step 6)  
**Flow:**

1. Operator checks that the Job targets the name of a configured Scale Set.
2. Use case ends.

## Postconditions

### Success Postconditions

- The Operator knows whether every Scale Set is served, how busy the Slots are, and the lifecycle of the Job looked up.

### Failure Postconditions

- Monitoring never changes the state of the Controller, its Scale Sets, or its Jobs.
- No secret is revealed by the health status, the metrics, or the logs.

## Business Rules

### BR-001: Health Threshold

The Controller reports unhealthy as UC-001 BR-005 defines it.

### BR-002: Host-Only Access

The health status and the metrics are reachable from the Host only.

### BR-003: No Secrets in Logs

No JIT Config, credential, or private key appears in the logs at any log level.
