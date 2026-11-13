# Use Case: Refresh Runner Images

## Overview

**Use Case ID:** UC-006  
**Use Case Name:** Refresh Runner Images  
**Primary Actor:** Time  
**Secondary Actors:** Docker Engine  
**Goal:** Keep every Runner Image recent so that Runner Containers stay within GitHub's runner update window.  
**Trigger:** The refresh interval has passed since the last refresh, or the Controller starts and a Runner Image is missing (UC-001 Deploy Controller)  
**Status:** Draft  

**Requirements:** [FR-023, FR-027, FR-030, FR-032, FR-037, C-003](../requirements.md)

## Preconditions

- The Controller is running, or it is starting and has validated its Config (UC-001 step 4).

## Main Success Scenario

1. The refresh interval passes.
2. System builds the default Runner Image on the Host from the Dockerfile it ships with, using the newest official actions-runner base image.
3. System makes the new build the default Runner Image.
4. System pulls the newest version of every per-repository Runner Image in the Config.
5. System records for every Runner Image the time of its last successful refresh.
6. Runner Containers started from now on use the refreshed Runner Images, while running Runner Containers continue unaffected.
7. System removes every Runner Image the refresh superseded once no Runner Container uses it.

## Alternative Flows

### A1: Default Image Build Fails

**Trigger:** The build of the default Runner Image fails (step 2)  
**Flow:**

1. System logs the failure.
2. System keeps the existing default Runner Image and retries the build one day later.
3. When the default Runner Image is overdue, System logs a warning naming it and reports unhealthy until a build succeeds.
4. Use case continues at step 4.

### A2: No Default Image to Fall Back On

**Trigger:** The build fails while no default Runner Image exists yet (step 2)  
**Flow:**

1. System logs the failure and reports it to UC-001 Deploy Controller.
2. Use case ends.

### A3: Per-Repository Image Pull Fails

**Trigger:** The pull of a per-repository Runner Image that is present on the Host fails (step 4)  
**Flow:**

1. System logs the failure naming the repository.
2. System keeps the existing image for that repository and retries the pull one day later.
3. When that Runner Image is overdue, System logs a warning naming it and reports unhealthy until a pull succeeds.
4. Use case continues at step 5.

### A4: Missing Per-Repository Image Cannot Be Pulled

**Trigger:** The pull of a per-repository Runner Image that is not yet on the Host fails (step 4)  
**Flow:**

1. System logs the failure naming the repository and reports it to UC-001 Deploy Controller, which retries the pull with growing delays.
2. Use case continues at step 5.

### A5: Superseded Image Cannot Be Removed

**Trigger:** The Docker Engine refuses to remove a superseded Runner Image (step 7)  
**Flow:**

1. System logs the failure naming the Runner Image.
2. System retries the removal at the next refresh.
3. Use case ends.

### A6: Runner Image Missing at Startup

**Trigger:** The Controller starts and a Runner Image the Config uses is not on the Host (step 1)  
**Flow:**

1. System builds the default Runner Image if it is missing and pulls every missing per-repository Runner Image, leaving present Runner Images as they are.
2. System records for every Runner Image it built or pulled the time of its successful refresh.
3. Use case ends; UC-001 Deploy Controller continues, and the next refresh of every Runner Image falls due as before.

## Postconditions

### Success Postconditions

- The default Runner Image is built from the newest official base image.
- Every per-repository Runner Image is the newest version available.
- The next refresh is due one refresh interval later.
- No Runner Image is overdue.
- No superseded Runner Image remains that no Runner Container uses.

### Failure Postconditions

- An existing Runner Image is never replaced by an incomplete or failed build.
- Running Runner Containers are never interrupted.
- A failed refresh is retried within one day.
- An overdue Runner Image is reported as unhealthy until a refresh of it succeeds.

## Business Rules

### BR-001: Refresh Interval

Runner Images are refreshed every configurable number of days, 7 by default.

### BR-002: Daily Retry

A failed build or pull of a Runner Image that is present on the Host is retried daily until it succeeds. A Runner Image that is missing is retried with growing delays instead, because its repository cannot be served (UC-001 A6).

### BR-003: Update Window

A Runner Image whose last successful refresh lies more than 49 days back is overdue. While it is overdue, the Controller reports unhealthy, which leaves the Operator 14 days to fix the refresh before GitHub's 63-day runner update window closes.

### BR-004: Default Runner Image Content

The default Runner Image adds the compose plugin and an SSH client to the official actions-runner image, with runner self-update disabled.

### BR-005: Entrypoint Supplied by the Controller

A Runner Image needs no entrypoint of its own; the Controller supplies it to every Runner Container.

### BR-006: Superseded Images

A Runner Image that a refresh superseded is removed once no Runner Container uses it; a Runner Image another program on the Host uses is never removed.
