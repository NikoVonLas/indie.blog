#!/usr/bin/env python3
"""Build the import payload for the formatting guide from its Markdown source."""
import json
from pathlib import Path

source = Path(__file__).resolve().parents[1] / "examples" / "formatting.md"
print(json.dumps([{
    "id": "example-formatting-guide", "slug": "vozmozhnosti-oformleniya",
    "kind": "blog", "lang": "ru", "title": "Что умеет этот блог",
    "subtitle": "Живая шпаргалка: от зачёркивания и спойлеров до видео и Markdown для ИИ.",
    "summary": "Все доступные способы оформления с работающими примерами и синтаксисом.",
    "body": source.read_text(), "image": "/static/demo-cover.svg",
    "author": "Демонстрационная статья", "visible": True, "commentable": True,
    "on_home": True, "published_at": "2026-10-13T12:00:00Z", "data": {}
}], ensure_ascii=False))
