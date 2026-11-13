# Glossary

| Term                | Definition                                                                                                                          | Avoid                      |
|---------------------|-------------------------------------------------------------------------------------------------------------------------------------|----------------------------|
| Controller          | The scaleset-runner process that polls GitHub for all Scale Sets and starts and removes Runner Containers.                           | Agent, Manager, Listener   |
| Instance ID         | The identifier that tells one Controller apart from another on the same Host; it labels its Runner Containers, its Scale Sets (Instance label), and its State Store rows. |                            |
| Instance label      | The label `scaleset-runner-<Instance ID>` the Controller adds to every Scale Set it creates, so that it recognizes its own Scale Sets even without the State Store. |                            |
| Config              | The environment variables the Controller reads at startup: one JSON variable for repositories and overrides, plus limits and credentials.| Config file                |
| State Store         | The SQLite database of one Controller that records the Scale Sets, Runner Containers, and Runner Registrations it created.          | DB                         |
| Scale Set           | A GitHub runner scale set registered on one repository; its name is the label workflows target with `runs-on`.                       | Runner group, Pool         |
| Message session     | The long-poll session the Controller holds with GitHub for one Scale Set to receive Job offers, Job events, and statistics; a successful poll renews it. |                            |
| Health threshold    | The time a Scale Set may go without a successful poll before the Controller reports unhealthy; 120 seconds by default, configurable.|                            |
| Job                 | One GitHub Actions workflow job assigned to a Scale Set.                                                                             | Task                       |
| Acquire             | The Controller's claim on a Job GitHub offers to a Scale Set; only an acquired Job is assigned to the Scale Set.                      | Accept                     |
| Assigned Job count  | The number of Jobs assigned to a Scale Set and not yet completed, as GitHub reports it in the statistics of a poll.                  |                            |
| Workflow run        | One run of a GitHub Actions workflow, triggered by an event in a configured repository; it consists of one or more Jobs.             |                            |
| Runner Registration | The ephemeral runner GitHub knows about for one Runner Container, created by a JIT Config and deleted with the Runner Container.     | Agent                      |
| Runner Container    | A single-use container, started from the Runner Image with a JIT Config, that executes exactly one Job, chosen by GitHub among the Scale Set's assigned Jobs, and is then removed. The Controller learns which Job from the JobStarted message. | Runner pod, Worker         |
| Idle Runner Container | A Runner Container whose runner no JobStarted message has named yet. GitHub may already have handed it a Job; only GitHub's answer to deleting its Runner Registration tells for sure. |                            |
| Start request       | A Scale Set's need for one more Runner Container, which arises while its assigned Job count exceeds its Runner Containers and waits for a free Slot. | Held Job, Queued Job       |
| Ready marker        | The line the Controller-supplied entrypoint writes to the Runner Container's output once the inner Docker daemon accepts commands and the runner has connected to GitHub. |                            |
| Readiness timeout   | The time a Runner Container may take to write the Ready marker before the Controller treats it as failed to start; 120 seconds by default, configurable. |                            |
| Growing delays      | Retry delays that start at 1 second, double with each failed attempt, vary randomly by up to 20%, and stop growing at 5 minutes.     | Backoff                    |
| Runner Image        | The container image Runner Containers start from: by default built on the Host from the official actions-runner image plus the compose plugin and an SSH client; the Controller supplies its entrypoint.|                            |
| Refresh interval    | The number of days between two refreshes of the Runner Images; 7 by default, configurable.                                          |                            |
| JIT Config          | The one-time runner configuration GitHub issues for one Runner Container; a secret until consumed.                                  | Registration token         |
| Slot                | One unit of a Controller's limit on Runner Containers that may run at the same time across all its Scale Sets.                      |                            |
| Capacity            | The largest number of Runner Containers the Controller tells GitHub a Scale Set may have at once: always the Slot limit. GitHub reads it with every poll. | Max runners                |
| Shutdown timeout    | The time running Jobs may take to finish after the Controller is asked to stop; 5 minutes by default, configurable.                  |                            |
| Fork pull request   | A pull request whose head branch lives in a fork of the repository; its workflows run code the repository owner has not reviewed.    |                            |
| Host                | The Docker host the Controller and its Runner Containers run on, initially orpheus.                                                  | Node                       |
| Operator            | The person who configures, deploys, and monitors the Controller.                                                                     | Admin                      |
| Workflow Author     | A person who writes GitHub Actions workflows in a configured repository and targets a Scale Set.                                     | Developer                  |
