# Claude's Memory Bank

My memory resets completely between sessions, so I rely entirely on the Memory Bank in [memory-bank/](memory-bank/) to understand this project. **At the start of every task, read ALL memory bank files.** This is not optional.

## Structure
The core files build on each other in this order:
1. [projectbrief.md](memory-bank/projectbrief.md): requirements, goals and scope; the source of truth
2. [productContext.md](memory-bank/productContext.md): why it exists, the problems it solves, UX goals
3. [systemPatterns.md](memory-bank/systemPatterns.md): architecture, key decisions, patterns, critical paths
4. [techContext.md](memory-bank/techContext.md): stack, setup, config, constraints, tool gotchas
5. [activeContext.md](memory-bank/activeContext.md): current focus, recent changes, next steps, preferences
6. [progress.md](memory-bank/progress.md): what works, what's left, known issues, how decisions evolved

Additional context: [deployment.md](memory-bank/deployment.md) (Kubernetes topology and procedures). Add more files under `memory-bank/` when a feature, integration, API or test strategy needs its own documentation.

## When to update
1. After discovering new project patterns
2. After implementing significant changes
3. When the user says **update memory bank**: review ALL files, even ones that need no change, and pay most attention to `activeContext.md` and `progress.md`
4. When the context needs clarification

Keep it precise: the Memory Bank is the only link to previous work. Write dates as absolute dates, and mark anything unverified as unverified.
