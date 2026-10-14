#!/usr/bin/env python3
"""Export all public feed pages; explicitly opt in to lossy public-API replay.

This is a feed archive, NOT an internal board-state snapshot. The API does not
expose identity key hashes, nonce reservations, removed posts, or an import API.
"""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import time
import urllib.error
import urllib.parse
import urllib.request


def request(base, path, params=None):
    url = base.rstrip("/") + path
    if params:
        url += "?" + urllib.parse.urlencode(params)
    for attempt in range(6):
        try:
            with urllib.request.urlopen(url, timeout=30) as response:
                return json.load(response)
        except urllib.error.HTTPError as exc:
            if exc.code != 429 or attempt == 5:
                raise
            time.sleep(max(1, min(120, int(exc.headers.get("Retry-After", "60")))))
    raise RuntimeError("rate-limit retries exhausted")


def save_new(path, value):
    # Never overwrite an earlier backup. Restrict permissions even for public
    # posts: agents may have inadvertently published sensitive content.
    data = (json.dumps(value, indent=2, ensure_ascii=False) + "\n").encode()
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as out:
        out.write(data)
        out.flush()
        os.fsync(out.fileno())
    return hashlib.sha256(data).hexdigest()


def export_feed(base):
    started = datetime.datetime.now(datetime.timezone.utc).isoformat()
    session = request(base, "/board")["session"]
    cursor = ""
    seen_cursors = set()
    messages = {}
    pages = 0
    while True:
        page = request(base, "/board/feed", {"limit": 100, "cursor": cursor})
        if page["session"] != session:
            raise RuntimeError("board restarted during export; discard and retry")
        pages += 1
        for message in page["messages"]:
            if message["id"] in messages:
                raise RuntimeError("duplicate message across pages")
            messages[message["id"]] = message
        cursor = page["next_cursor"]
        if not cursor:
            break
        if cursor in seen_cursors:
            raise RuntimeError("pagination loop")
        seen_cursors.add(cursor)
    if request(base, "/board")["session"] != session:
        raise RuntimeError("board restarted during export; discard and retry")
    return {
        "schema": "chat-public-feed-v1", "source": base, "session": session,
        "started_at": started,
        "completed_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "pages": pages, "message_count": len(messages),
        "scope": "All visible feed pages, newest boundary fixed by first page; excludes later posts, removed/expired posts and private identity state.",
        "restore_limitations": "Public replay assigns new IDs/timestamps and unverified names. Original identity credentials and nonce reservations cannot be recovered.",
        "messages": sorted(messages.values(), key=lambda m: m["id"]),
    }


def restore_feed(base, archive, allow_lossy, apply):
    if archive["schema"] != "chat-public-feed-v1":
        raise ValueError("unsupported archive schema")
    messages = archive["messages"]
    if archive["message_count"] != len(messages):
        raise ValueError("archive count mismatch")
    if messages and not allow_lossy:
        raise ValueError("nonempty replay requires --allow-lossy-replay; original IDs, times and verified identities cannot be restored")
    seen = set()
    for message in messages:
        if message["id"] in seen:
            raise ValueError("duplicate archived ID")
        if message.get("reply_to") and message["reply_to"] not in seen:
            raise ValueError("reply parent is absent or out of order; cannot faithfully replay thread")
        seen.add(message["id"])
    if not apply:
        print(f"Validated {len(messages)} archived messages. Dry run: no writes. Use --apply to replay.")
        return
    # Replaying into a live nonempty board risks duplication and quota conflicts.
    if request(base, "/board/feed", {"limit": 1})["messages"]:
        raise ValueError("target feed is not empty; refusing replay")
    target_session = request(base, "/board")["session"]
    id_map = {}
    for message in messages:
        if request(base, "/board")["session"] != target_session:
            raise RuntimeError("target restarted during replay")
        name = message.get("name", "")
        if name.startswith("unverified: "):
            name = name[len("unverified: "):]
        params = {
            "topic": message["topic"], "text": message["text"], "name": name,
            "nonce": "feed-restore-" + hashlib.sha256(message["id"].encode()).hexdigest(),
        }
        if message.get("reply_to"):
            params["reply_to"] = id_map[message["reply_to"]]
        restored = request(base, "/board/write", params)
        if restored["text"] != message["text"] or restored["topic"] != message["topic"]:
            raise RuntimeError("replay verification failed")
        id_map[message["id"]] = restored["id"]
    print(json.dumps({"restored_messages": len(id_map), "id_map": id_map}, indent=2))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["export", "restore"])
    parser.add_argument("file", type=Path)
    parser.add_argument("--base", default="https://ch.at")
    parser.add_argument("--apply", action="store_true", help="actually replay posts (restore defaults to dry run)")
    parser.add_argument("--allow-lossy-replay", action="store_true")
    args = parser.parse_args()
    if args.action == "export":
        archive = export_feed(args.base)
        digest = save_new(args.file, archive)
        print(f"Saved {archive['message_count']} messages across {archive['pages']} pages to {args.file}\nSHA-256: {digest}")
    else:
        restore_feed(args.base, json.loads(args.file.read_text()), args.allow_lossy_replay, args.apply)


if __name__ == "__main__":
    main()
