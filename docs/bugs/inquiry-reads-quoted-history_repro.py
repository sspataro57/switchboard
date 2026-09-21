#!/usr/bin/env python3
"""Model-in-the-loop reproduction for SWT-70 / inquiry-reads-quoted-history.

Replays the STORED inquiry-lane prompt for one message against the same local
model, with the same options internal/provider/ollama.go sends (/api/chat,
stream false, think false, temperature 0, num_predict 512, format = the lane's
schema), N times, and prints needs_reply / ask_kind / reason for each run.

Then it replays two edits of the same prompt:
  - "no-quote-target":  the message-to-decide-on body cut at its first reply
                        separator, so only the new text is judged.
  - "no-quote-anywhere": the same, plus every flattened thread-context line
                        truncated at its own separator.

Read-only: nothing is written to any database, and no verdict is stored. The
system prompt and JSON schema are read out of internal/classify/inquiry.go at
run time, so this cannot drift from the lane.

Get the stored prompt first (production db is read-only for this):

  psql "$OPS_DATABASE_URL" -At -o /tmp/prompt-385428.txt \
    -c "select input->>'user_prompt' from ai_runs where id=12721;"

Run:

  python3 docs/bugs/inquiry-reads-quoted-history_repro.py /tmp/prompt-385428.txt --n 5

Environment: OPS_LOCAL_PROVIDER_URL (default http://192.168.50.55:11434),
OPS_LOCAL_MODEL (default qwen3:8b). Expect ~10 s per verdict on the z4.
"""

import argparse
import json
import os
import re
import sys
import urllib.request

REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
INQUIRY_GO = os.path.join(REPO, "internal", "classify", "inquiry.go")

DECIDE_MARKER = "The message to decide on:"
SEPARATORS = [
    "-----Original Message-----",
    "-----Mensaje original-----",
]
QUOTE_LINE = re.compile(r"^On .*wrote:\s*$")


def lane_prompt_and_schema(path):
    """Pull InquirySystemPrompt and InquiryVerdictSchema out of the Go source."""
    src = open(path, encoding="utf-8").read()
    sys_m = re.search(r"const InquirySystemPrompt = `(.*?)`", src, re.S)
    schema_m = re.search(r"InquiryVerdictSchema = json\.RawMessage\(`(.*?)`\)", src, re.S)
    if not sys_m or not schema_m:
        sys.exit("could not read InquirySystemPrompt / InquiryVerdictSchema from " + path)
    return sys_m.group(1), json.loads(schema_m.group(1))


def cut_at_separator(text):
    """Everything before the first reply separator (any of the three shapes)."""
    idx = len(text)
    for sep in SEPARATORS:
        i = text.find(sep)
        if i >= 0:
            idx = min(idx, i)
    lines = text.split("\n")
    off = 0
    for line in lines:
        if QUOTE_LINE.match(line) or line.startswith(">"):
            idx = min(idx, off)
            break
        off += len(line) + 1
    return text[:idx].rstrip() + "\n"


def variant_no_quote_target(prompt):
    i = prompt.find(DECIDE_MARKER)
    if i < 0:
        return cut_at_separator(prompt)
    head, tail = prompt[:i], prompt[i:]
    return head + cut_at_separator(tail)


def variant_no_quote_anywhere(prompt):
    i = prompt.find(DECIDE_MARKER)
    head, tail = (prompt[:i], prompt[i:]) if i >= 0 else (prompt, "")
    out = []
    for line in head.split("\n"):
        if line.startswith("me: ") or line.startswith("them: "):
            tag, body = line.split(" ", 1)
            line = tag + " " + cut_at_separator(body).strip()
        out.append(line)
    return "\n".join(out) + cut_at_separator(tail)


def ask(base_url, model, system, schema, user, timeout):
    body = {
        "model": model,
        "messages": [
            {"role": "system", "content": system},
            {"role": "user", "content": user},
        ],
        "format": schema,
        "options": {"num_predict": 512, "temperature": 0},
        "keep_alive": "30m",
        "think": False,
        "stream": False,
    }
    req = urllib.request.Request(
        base_url.rstrip("/") + "/api/chat",
        data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        env = json.loads(resp.read().decode("utf-8"))
    if env.get("error"):
        raise RuntimeError(env["error"])
    return json.loads(env["message"]["content"])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("prompt_file", help="file holding ai_runs.input->>'user_prompt'")
    ap.add_argument("--n", type=int, default=5)
    ap.add_argument("--timeout", type=int, default=180)
    ap.add_argument(
        "--variants",
        default="stored,no-quote-target,no-quote-anywhere",
        help="comma-separated subset of stored,no-quote-target,no-quote-anywhere",
    )
    args = ap.parse_args()

    base_url = os.environ.get("OPS_LOCAL_PROVIDER_URL", "http://192.168.50.55:11434")
    model = os.environ.get("OPS_LOCAL_MODEL", "qwen3:8b")
    system, schema = lane_prompt_and_schema(INQUIRY_GO)
    # newline="" so CRLF line endings stored by the mail path reach the model
    # exactly as they did in the original run.
    stored = open(args.prompt_file, encoding="utf-8", newline="").read()

    builders = {
        "stored": lambda p: p,
        "no-quote-target": variant_no_quote_target,
        "no-quote-anywhere": variant_no_quote_anywhere,
    }

    print("model      %s @ %s" % (model, base_url))
    print("prompt     %s (%d chars)" % (args.prompt_file, len(stored)))
    print()

    for name in [v.strip() for v in args.variants.split(",") if v.strip()]:
        user = builders[name](stored)
        print("== %s (%d chars) ==" % (name, len(user)))
        for i in range(args.n):
            try:
                v = ask(base_url, model, system, schema, user, args.timeout)
            except Exception as exc:  # network, model, or malformed JSON
                print("  run %d: ERROR %s" % (i + 1, exc))
                continue
            print(
                "  run %d: needs_reply=%-5s ask_kind=%-10s ask=%r"
                % (i + 1, v.get("needs_reply"), v.get("ask_kind"), v.get("ask", "")[:80])
            )
            print("          reason=%s" % v.get("reason", "")[:220])
        print()


if __name__ == "__main__":
    main()
