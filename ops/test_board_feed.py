import contextlib
import importlib.util
import io
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("board_feed", Path(__file__).with_name("board-feed.py"))
feed = importlib.util.module_from_spec(spec)
spec.loader.exec_module(feed)


class FeedArchiveTests(unittest.TestCase):
    def test_pagination_and_order(self):
        responses = [{"session": "s"},
                     {"session": "s", "messages": [{"id": "s-2"}], "next_cursor": "s-2"},
                     {"session": "s", "messages": [{"id": "s-1"}], "next_cursor": ""},
                     {"session": "s"}]
        with patch.object(feed, "request", side_effect=responses) as request:
            archive = feed.export_feed("https://ch.at")
        self.assertEqual(archive["message_count"], 2)
        self.assertEqual(archive["pages"], 2)
        self.assertEqual([m["id"] for m in archive["messages"]], ["s-1", "s-2"])
        self.assertEqual(request.call_args_list[2].args[2]["cursor"], "s-2")

    def test_restart_fails_closed(self):
        with patch.object(feed, "request", side_effect=[{"session": "s"}, {"session": "new"}]):
            with self.assertRaisesRegex(RuntimeError, "restarted"):
                feed.export_feed("https://ch.at")

    def test_backup_never_overwrites(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "feed.json"
            feed.save_new(path, {"messages": []})
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(FileExistsError):
                feed.save_new(path, {"messages": ["overwrite"]})

    def test_replay_remaps_threads_without_claiming_identity(self):
        archive = {"schema": "chat-public-feed-v1", "message_count": 2, "messages": [
            {"id": "old-1", "topic": "test", "text": "parent", "name": "agent", "actor_id": "original", "verified_same_actor": True},
            {"id": "old-2", "topic": "test", "text": "reply", "reply_to": "old-1"},
        ]}
        writes = []
        def request(base, path, params=None):
            if path == "/board/feed":
                return {"messages": []}
            if path == "/board":
                return {"session": "new"}
            writes.append(params)
            return dict(params, id=f"new-{len(writes)}")
        with patch.object(feed, "request", side_effect=request), contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(ValueError, "allow-lossy"):
                feed.restore_feed("https://ch.at", archive, False, True)
            feed.restore_feed("https://ch.at", archive, True, False)
            self.assertEqual(writes, [])
            feed.restore_feed("https://ch.at", archive, True, True)
        self.assertEqual(writes[1]["reply_to"], "new-1")
        self.assertNotIn("actor", writes[0])
        self.assertNotIn("key", writes[0])

    def test_empty_archive_restores_without_writes(self):
        archive = {"schema": "chat-public-feed-v1", "message_count": 0, "messages": []}
        with patch.object(feed, "request", side_effect=[{"messages": []}, {"session": "new"}]) as request, contextlib.redirect_stdout(io.StringIO()):
            feed.restore_feed("https://ch.at", archive, False, True)
        self.assertEqual(request.call_count, 2)

    def test_nonempty_target_refused(self):
        archive = {"schema": "chat-public-feed-v1", "message_count": 0, "messages": []}
        with patch.object(feed, "request", return_value={"messages": [{"id": "existing"}]}):
            with self.assertRaisesRegex(ValueError, "not empty"):
                feed.restore_feed("https://ch.at", archive, False, True)


if __name__ == "__main__":
    unittest.main()
