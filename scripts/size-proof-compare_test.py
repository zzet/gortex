#!/usr/bin/env python3
"""Exercise the comparator's actual integer gate without artifact I/O."""
import ast
from pathlib import Path
import unittest


def comparator_expressions():
    script = Path(__file__).with_name('size-proof-compare.py')
    module = ast.parse(script.read_text())
    for node in ast.walk(module):
        if not isinstance(node, ast.Dict):
            continue
        fields = {key.value: value for key, value in zip(node.keys, node.values)
                  if isinstance(key, ast.Constant) and isinstance(key.value, str)}
        gate = fields.get('size_gate_passed', fields.get('two_percent_gate_passed'))
        maximum = fields.get('maximum_bytes_at_limit',
                             fields.get('maximum_bytes_at_two_percent'))
        if gate is not None and maximum is not None:
            return (compile(ast.Expression(gate), str(script), 'eval'),
                    compile(ast.Expression(maximum), str(script), 'eval'))
    raise AssertionError('comparator size gate and maximum are missing')


class ReleaseSizeLimitTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.gate, cls.maximum = comparator_expressions()

    def test_five_percent_integer_boundaries(self):
        for arch, published, ceiling in (
                ('amd64', 308541560, 323968638),
                ('arm64', 303859576, 319052554)):
            with self.subTest(arch=arch):
                values = {'tag_bytes': published, 'csize': ceiling}
                self.assertEqual(eval(self.maximum, {}, values), ceiling)
                self.assertTrue(eval(self.gate, {}, values))
                values['csize'] += 1
                self.assertFalse(eval(self.gate, {}, values))

    def test_existing_matched_candidates_fit_approved_limit(self):
        for arch, published, candidate in (
                ('amd64', 308541560, 322893720),
                ('arm64', 303859576, 316479096)):
            with self.subTest(arch=arch):
                self.assertTrue(eval(self.gate, {}, {
                    'tag_bytes': published, 'csize': candidate}))


if __name__ == '__main__':
    unittest.main()

