"""Codex-shaped terminal boundary for the dead-letter/replay integration proof."""
import json
import os
import sys
import termios
import threading
import time
import tty

draft = bytearray()
lock = threading.Lock()
busy = False
log_path = sys.argv[1]


def render():
    while True:
        with lock:
            text = draft.decode("utf-8", "replace")
            if len(text) > 200:
                text = "[Pasted Content %d chars]" % len(text)
            elif not text:
                text = "\x1b[2mExplain this codebase\x1b[22m"
            frame = "\x1b[2J\x1b[Hfake-codex-tui\r\n\r\n"
            if busy:
                frame += "• Working (1s • esc to interrupt)\r\n"
            else:
                frame += "\r\n"
            frame += "› " + text.replace("\n", "\r\n") + "\r\n\r\n  gpt-5.5 low · /tmp/probe\r\n"
        sys.stdout.write(frame)
        sys.stdout.flush()
        time.sleep(0.05)


fd = sys.stdin.fileno()
previous = termios.tcgetattr(fd)
tty.setraw(fd)
sys.stdout.write("\x1b[?2004h")
sys.stdout.flush()
threading.Thread(target=render, daemon=True).start()
escape = bytearray()
pasting = False
try:
    while True:
        ch = os.read(fd, 1)
        if not ch:
            break
        if ch == b"\x1b":
            escape = bytearray(ch)
            continue
        if escape:
            escape.extend(ch)
            if any(marker.startswith(escape) for marker in (b"\x1b[200~", b"\x1b[201~")):
                if escape == b"\x1b[200~":
                    pasting = True
                    escape.clear()
                elif escape == b"\x1b[201~":
                    pasting = False
                    escape.clear()
                continue
            escape.clear()
        with lock:
            if pasting:
                draft.extend(ch)
            elif ch == b"\x15":
                draft.clear()
            elif ch in (b"\r", b"\n"):
                if draft:
                    with open(log_path, "a") as log:
                        log.write("SUBMIT\t" + json.dumps(draft.decode("utf-8", "replace")) + "\n")
                    draft.clear()
                    busy = True
            else:
                draft.extend(ch)
finally:
    termios.tcsetattr(fd, termios.TCSADRAIN, previous)
