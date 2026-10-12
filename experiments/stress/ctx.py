import json, sys
from stress import phase, TOPICS
k, words = int(sys.argv[1]), int(sys.argv[2])
items = ", ".join(TOPICS[:k])
prompt = (f"Split this into exactly {k} parallel subtasks, one per topic (this is a context-limit test). "
          f"Each subtask must write a detailed {words}-word technical article with code examples. Topics: {items}.")
phase(f"ctx{k}x{words}", [(prompt, 1, k)])
