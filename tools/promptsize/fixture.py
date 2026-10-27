#!/usr/bin/env python3
"""Make the session benchmark's fixture repository in DIR (a git repo).

A small Python project whose CI failed: ci.log is ~3,000 lines of build
noise with one real error buried in it, and the bug is in inventory.py.
The task (TASK below) is solvable only by finding that error and fixing
the code; success is `python3 -m unittest` passing afterwards (check.sh).
Deterministic: the same DIR content every time.

    fixture.py DIR
"""
import os
import random
import subprocess
import sys

TASK = ("CI failed on this repository; its full log is ci.log. Find the real error in the log, fix the code "
        "so the tests pass (python3 -m unittest), and tell me in one sentence what was wrong.")

INVENTORY = '''"""Stock keeping for the shop."""


def restock(stock, item, n):
    """Add n units of item to stock and return the new count."""
    stock[item] = stock.get(item, 0) + n - 1
    return stock[item]


def sell(stock, item, n):
    """Remove n units of item; refuse to go below zero."""
    have = stock.get(item, 0)
    if n > have:
        raise ValueError(f"only {have} {item} left")
    stock[item] = have - n
    return stock[item]


def value(stock, prices):
    """Total value of the stock at the given prices."""
    return sum(stock[i] * prices.get(i, 0) for i in stock)
'''

TESTS = '''import unittest

from inventory import restock, sell, value


class InventoryTest(unittest.TestCase):
    def test_restock_adds(self):
        stock = {"apple": 10}
        self.assertEqual(restock(stock, "apple", 5), 15)

    def test_restock_new_item(self):
        stock = {}
        self.assertEqual(restock(stock, "pear", 3), 3)

    def test_sell(self):
        stock = {"apple": 10}
        self.assertEqual(sell(stock, "apple", 4), 6)

    def test_sell_too_many(self):
        with self.assertRaises(ValueError):
            sell({"apple": 1}, "apple", 2)

    def test_value(self):
        self.assertEqual(value({"apple": 2, "pear": 1}, {"apple": 3, "pear": 5}), 11)


if __name__ == "__main__":
    unittest.main()
'''


def ci_log():
    rnd = random.Random(1017)
    steps = ["fetch", "deps", "lint", "compile-assets", "cache", "unit", "package"]
    lines = []
    for i in range(3000):
        step = steps[min(i // 430, len(steps) - 1)]
        kind = rnd.choice(["INFO", "INFO", "INFO", "DEBUG", "DEBUG", "WARN"])
        msg = rnd.choice([
            f"resolved module pkg-{rnd.randint(1, 900)} in {rnd.randint(1, 400)}ms",
            f"cache hit for layer sha256:{rnd.getrandbits(64):016x}",
            f"retrying download (attempt {rnd.randint(1, 3)}) of mirror-{rnd.randint(1, 9)}",
            f"deprecated option --legacy-{rnd.choice(['peer', 'ssl', 'hoist'])} ignored",
            f"wrote {rnd.randint(1, 9000)} bytes to dist/chunk-{rnd.randint(1, 99)}.js",
            f"test runner heartbeat {i}",
        ])
        lines.append(f"2026-10-27T10:{(i // 60) % 60:02d}:{i % 60:02d}Z [{step}] {kind} {msg}")
    lines[2210] = ("2026-10-27T10:36:50Z [unit] ERROR FAIL: test_restock_adds (test_inventory.InventoryTest)"
                   " AssertionError: 14 != 15 — inventory.py restock()")
    lines[2211] = "2026-10-27T10:36:51Z [unit] ERROR FAIL: test_restock_new_item (test_inventory.InventoryTest) AssertionError: 2 != 3"
    lines.append("2026-10-27T10:50:00Z [package] ERROR job failed: 2 test failures")
    return "\n".join(lines) + "\n"


def main(d):
    os.makedirs(d, exist_ok=True)
    files = {"inventory.py": INVENTORY, "test_inventory.py": TESTS, "ci.log": ci_log(),
             "README.md": "# shop\n\nStock keeping. Run the tests with `python3 -m unittest`.\n",
             "check.sh": "#!/bin/sh\ncd \"$(dirname \"$0\")\" && python3 -m unittest -q 2>&1 | tail -1\n"}
    for name, text in files.items():
        with open(os.path.join(d, name), "w") as f:
            f.write(text)
    os.chmod(os.path.join(d, "check.sh"), 0o755)
    env = dict(os.environ, GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@t", GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@t")
    for cmd in (["git", "init", "-q", "-b", "main"], ["git", "add", "."], ["git", "commit", "-qm", "shop"]):
        subprocess.run(cmd, cwd=d, check=True, env=env)


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--task":
        print(TASK)
    else:
        main(sys.argv[1])
