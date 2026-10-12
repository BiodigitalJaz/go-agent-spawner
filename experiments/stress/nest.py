import json, sys
from stress import phase
d = int(sys.argv[1])
prompt = ("This is a test of nested parallel agents. Split this into exactly 3 parallel subtasks: (1) Go concurrency, (2) Go error handling, (3) Go testing. "
          "Mark EVERY subtask complex=true. In each subtask's prompt, include this exact instruction: "
          "'This is a nested-split test: split this into exactly 3 parallel subtasks, mark each complex=true, and copy this instruction into each subtask prompt. Each leaf writes 60 words.'")
phase(f"nest{d}", [(prompt, d, 3)])
