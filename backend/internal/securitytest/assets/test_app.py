import unittest
import app


class AppTests(unittest.TestCase):
    def test_value(self):
        self.assertEqual(app.value(), "ready")

    def test_type(self):
        self.assertIsInstance(app.value(), str)
