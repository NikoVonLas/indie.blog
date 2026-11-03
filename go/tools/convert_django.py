#!/usr/bin/env python3
"""Convert `manage.py dumpdata posts.Post` on stdin to the Go post import format.

This deliberately converts publications only. Users, comments, subscriptions and
media files require separate migration; their absence must not be mistaken for a
complete backup. No network requests or database writes are performed here.
"""
import json
import sys
from datetime import datetime, timezone


def timestamp(value):
    # Upstream uses USE_TZ=False and TIME_ZONE=UTC. dumpdata therefore writes
    # timestamps without an offset, while Go's time.Time expects RFC3339.
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")


def convert(records):
    result = []
    for record in records:
        if record.get("model", "").lower() != "posts.post":
            continue
        f = record["fields"]
        data = f.get("data")
        extra = dict(data) if isinstance(data, dict) else {"legacy_data": data}
        extra["legacy_metadata"] = {
            key: f.get(key) for key in ("og_title", "og_image", "og_description", "announce_text", "word_count", "comment_count")
        }
        result.append({
            "id": str(record["pk"]), "slug": f["slug"], "kind": f["type"],
            "lang": f.get("lang", "ru"), "title": f.get("title") or "",
            "subtitle": f.get("subtitle") or "", "body": f.get("text") or (f.get("html_cache") if f.get("is_raw_html") else "") or "",
            "summary": f.get("og_description") or f.get("announce_text") or "",
            "image": f.get("image") or "", "author": f.get("author") or "",
            "external_url": f.get("url") or "", "css": f.get("css") or "",
            "raw_html": f.get("is_raw_html", False), "visible": f.get("is_visible", False),
            "members_only": f.get("is_members_only", False), "commentable": f.get("is_commentable", True),
            "on_home": f.get("is_visible_on_home_page", False),
            "published_at": timestamp(f["published_at"]), "created_at": timestamp(f["created_at"]), "updated_at": timestamp(f["updated_at"]),
            "views": f.get("view_count", 0), "data": extra,
        })
    return result


if __name__ == "__main__":
    json.dump(convert(json.load(sys.stdin)), sys.stdout, ensure_ascii=False, indent=2)
    print()
