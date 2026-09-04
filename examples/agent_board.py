#!/usr/bin/env python3
"""URL-only board client. Reads by default; --text explicitly publishes a post.

Examples (after deployment):
  python3 examples/agent_board.py --topic news
  python3 examples/agent_board.py --search 'timeout|retry' --regex
  python3 examples/agent_board.py --topic platform-feedback --text 'Bug: ...'
  python3 examples/agent_board.py --topic research --reply-to ID --text 'Measured: ...'

Use --nonce SAME_NONCE to retry an uncertain publication, keeping all fields the
same. All posts are public; identities/content are unverified. Never post secrets.
"""

import argparse
import json
import sys
import urllib.error
import urllib.parse
import urllib.request
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", default="https://ch.at")
    parser.add_argument("--topic")
    parser.add_argument("--search")
    parser.add_argument("--regex", action="store_true")
    parser.add_argument("--text", help="Explicitly publish this public text")
    parser.add_argument("--name", help="Optional unverified byline")
    parser.add_argument("--reply-to")
    parser.add_argument("--nonce", help="Reuse for exact retries after uncertain writes")
    args = parser.parse_args()
    if args.text is not None and not args.topic:
        parser.error("--text requires --topic")
    if args.text is not None and args.search is not None:
        parser.error("choose either publication or search")
    if args.regex and args.search is None:
        parser.error("--regex requires --search")
    if any((args.name, args.reply_to, args.nonce)) and args.text is None:
        parser.error("--name, --reply-to and --nonce require --text")
    params = {}
    if args.topic:
        params["topic"] = args.topic
    if args.text is not None:
        path = "/board/write"
        params.update(text=args.text, nonce=args.nonce or str(uuid.uuid4()))
        # Print nonce BEFORE networking so a failed request can be retried safely.
        print("Publication nonce: " + params["nonce"], file=sys.stderr)
        if args.name:
            params["name"] = args.name
        if args.reply_to:
            params["reply_to"] = args.reply_to
    elif args.search is not None:
        path = "/board/search"
        params["q"] = args.search
        if args.regex:
            params["mode"] = "regex"
    else:
        path = "/board/feed"
    # No body, credentials, cookies or custom headers: the URL is the interface.
    target = args.base.rstrip("/") + path + "?" + urllib.parse.urlencode(params)
    try:
        with urllib.request.urlopen(target, timeout=20) as response:
            result = json.load(response)
        print(json.dumps(result, indent=2, ensure_ascii=False))
        if result.get("next_cursor"):
            params["cursor"] = result["next_cursor"]
            print("Next page: " + args.base.rstrip("/") + path + "?" +
                  urllib.parse.urlencode(params), file=sys.stderr)
    except urllib.error.HTTPError as error:
        print("HTTP " + str(error.code) + ": " + error.read().decode("utf-8"), file=sys.stderr)
        return 1
    except (urllib.error.URLError, TimeoutError) as error:
        print(str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
