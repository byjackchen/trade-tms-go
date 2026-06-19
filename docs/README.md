# Documentation Index

Docs are grouped by purpose. Pick the folder that matches what you're doing.

| Folder | What's in it | Start here when… |
|---|---|---|
| [`reference/`](reference/) | Living reference: architecture, API contract, benchmarks, strategy-research constraints | You need to understand or build against the system |
| [`spec/`](spec/) | Locked specifications: data, engine, risk, each strategy, UI/CLI | You're implementing or changing a contract |
| [`design/`](design/) | Active design proposals / backlog (and `archive/` for completed ones) | You're planning a refactor or feature |
| [`deploy/`](deploy/) | First-time and rolling-update deployment playbooks | You're deploying or updating a node |
| [`runbooks/`](runbooks/) | Operational smoke tests and verification checklists | You're verifying a live/paper setup |

## Quick links by role

- **Strategy researcher** → [`reference/strategies-constraints.md`](reference/strategies-constraints.md) (what the platform can/can't do), then the relevant `spec/strategy-*.md`.
- **Operator deploying** → [`deploy/first-deploy-playbook.md`](deploy/first-deploy-playbook.md) (fresh) or [`deploy/redeploy-playbook.md`](deploy/redeploy-playbook.md) (updates).
- **Developer** → [`design/`](design/) for active proposals, then [`spec/`](spec/) for the affected rules.
- **API client** → [`reference/api.md`](reference/api.md) (contract) + [`spec/api-ws-redis.md`](spec/api-ws-redis.md) (transport detail).
- **Architecture deep-dive** → [`reference/architecture.md`](reference/architecture.md).

## Specs

Data & platform: [`data-sharadar`](spec/data-sharadar.md) · [`calendar-universe`](spec/calendar-universe.md) · [`domain-types-money`](spec/domain-types-money.md) · [`engine-fill-model`](spec/engine-fill-model.md) · [`portfolio-risk`](spec/portfolio-risk.md) · [`hyperopt-metrics`](spec/hyperopt-metrics.md) · [`api-ws-redis`](spec/api-ws-redis.md) · [`ui-runner-modes-eod`](spec/ui-runner-modes-eod.md)

Strategies: [`strategy-sepa`](spec/strategy-sepa.md) · [`strategy-sector-orb`](spec/strategy-sector-orb.md) · [`strategy-pairs`](spec/strategy-pairs.md)
