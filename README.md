# What is this

The goal:

- a herdr wrapper web UI
- oppinionated
- connect to the herdr local socket and:
  - start herdr agent session
  - run tasks
  - shut down when task is done
- a task can outlive many agent invications, the wrapper shall preserve memory / context across agent runs

