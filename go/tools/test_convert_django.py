import unittest
from convert_django import convert, timestamp


class ConversionTests(unittest.TestCase):
    def test_upstream_naive_utc(self):
        self.assertEqual(timestamp("2020-01-02T03:04:05.123"), "2020-01-02T03:04:05.123000Z")
        self.assertEqual(timestamp("2020-01-02T08:04:05+05:00"), "2020-01-02T03:04:05Z")

    def test_metadata_html_and_visibility_are_preserved(self):
        f = {"slug": "old", "type": "blog", "text": "", "is_raw_html": True,
             "html_cache": "<p>Old text</p>", "is_members_only": True, "is_visible": False,
             "og_title": "Social title", "data": [1, 2],
             "published_at": "2020-01-02T03:04:05", "created_at": "2020-01-02T03:04:05",
             "updated_at": "2020-01-02T03:04:05"}
        p = convert([{"model": "posts.post", "pk": "uuid", "fields": f}])[0]
        self.assertEqual(p["id"], "uuid")
        self.assertEqual(p["body"], "<p>Old text</p>")
        self.assertFalse(p["visible"])
        self.assertTrue(p["members_only"])
        self.assertEqual(p["data"]["legacy_metadata"]["og_title"], "Social title")
        self.assertEqual(p["data"]["legacy_data"], [1, 2])


if __name__ == "__main__":
    unittest.main()
