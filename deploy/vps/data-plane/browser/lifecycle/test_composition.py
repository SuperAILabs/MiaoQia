import unittest

from composition import drill


class CompositionInputs(unittest.TestCase):
    def test_rejects_shell_metacharacters_before_policy_installation(self):
        for address in ("192.0.2.1; touch fixture-marker", "$(id)", "192.0.2.1\nflush ruleset"):
            with self.subTest(address=address), self.assertRaises(ValueError):
                drill("unused", "unused", address, "192.0.2.2", "unused", "unused", "unused", "unused")
