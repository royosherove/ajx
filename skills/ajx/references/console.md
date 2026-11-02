# Review, start and watch a trial in the AJX Console

`ajx console` serves a local web page for one trial. You can review the plan, set the common
parameters, check readiness, start, stop, watch the run and read the results there. Your agent
sees the same trial from chat. The console runs as the user who started it, on `127.0.0.1` only.

## Try it without an agent or a model

```sh
python3 skills/ajx/bin/ajx console demo trials/console-demo
```

This copies a demo trial into `trials/console-demo` and opens it. The demo's worker is
simulated (`plugins/console_demo.py`): it prints a scripted session for a fictional
`orders-cli`, writes `out.csv`, and answers the reporting calls with canned text. Press
**Check and start**; a run takes about a minute. Set `AJX_DEMO_PACE=0.2` before opening the
console to make it faster.

## Open a trial

```sh
ajx console trials/my-cli                  # same as: ajx console open trials/my-cli/trial.toml
```

The command starts a background server, opens your browser and prints a link that carries a
private key. Run it again to reuse the server and print the link again. Use `--no-open` to print
the link only, or `--foreground` to keep the server in the terminal until Ctrl-C.
The server stops after 30 idle minutes, or with `ajx console stop trials/my-cli`.

Closing the page, the terminal or the chat never stops a run. Runs started from the page use the
environment of the process that opened the console, including its credential variables.

## What each tab does

| Tab | What you can do |
|---|---|
| Plan | Read the task prompt the worker receives and the success checks. See where the worker runs and how it signs in. Set the time limit, attempts, budget and models. Edit `trial.toml` directly; an edit is saved only if AJX can load it. Check readiness, then start or resume. |
| Live | Follow each attempt's stages, the worker's latest events, verification, cleanup and reporting, plus AJX's own output. Stop a run, or retry cleanup when it was not confirmed. |
| Results | Read the verified outcome and the asks with their evidence IDs. Open the report, the journey and `asks.md`. Export a run for a team, or rebuild the reports without model calls. |
| Activity | See everything you, your agent and the console did. Send a message to your agent. |

Each button runs an existing command, and the page shows it: `ajx validate`, `ajx doctor`,
`ajx run`, `ajx run --cells C --stages S`, `ajx report --no-synthesis` and `ajx export`. The page
adds no other way to execute a task.

Live worker events need an adapter that reports progress. Claude Code and the demo do. Other
agents show stages only until the report lists every event.

## Approval

Pressing Start approves one exact plan: the bytes of `trial.toml`, the task prompt, the starting
files and the trial's own `plugins/`. The page shows the plan ID. Before launching, the console
runs `ajx validate` and `ajx doctor`, unless both already passed for this exact plan, and it
starts only if both pass. Any later change to those files voids the approval.

- A plan that changed after the page showed it is refused, so a change your agent made meanwhile
  cannot be approved unseen.
- The launched `ajx run --expect-plan <id>` refuses to start if the files changed between approval
  and launch.
- Scripts that your checks call from elsewhere are not part of the plan.

## Output folders, resuming and stopping

- **Resume.** Start resumes an output folder that has unfinished attempts. It never runs a
  finished or stopped attempt again.
- **Change the task.** After attempts exist, the task prompt is locked for that folder. Choose
  **Use a fresh output folder**, which sets `trial.output_dir`, then edit the task.
- **Run again under the same conditions.** Choose **Add another attempt**. Start then runs only
  the new repetition in the same folder.
- **Stop.** Stop sends SIGINT to `ajx run`. AJX stops the worker, runs task cleanup, releases the
  environment and archives the evidence. Resume later to write the report from what was captured.
  A second Stop is refused while cleanup runs. `ajx run` treats SIGTERM and SIGHUP the same way.
  The page cannot stop a run with `parallel` above 1, because AJX would wait for every running
  attempt.

## For the coordinating agent

The console is useful when the user wants to review or run the trial in a browser. Open it after
the trial validates. Draft the task prompt, the success check and any fixture as usual; the page
does not write task text.

```sh
ajx console <trial-dir> --no-open      # prints the link; the server keeps running
```

Tell the user what needs their decision, and that you cannot see the page live. Then:

- **Catch up before every reply.** Run `ajx console since <trial> --cursor N`, passing the
  `cursor` from your previous call, or 0 the first time. Summarize `headline` and `changes` in a
  sentence or two; never paste the JSON. Answer each item in `messages_for_agent`, and when the
  user is in the browser, post a short answer with `ajx console note <trial> "..." --reply-to <n>`.
- **Never approve, start, stop or retry for the user.** If they approve in chat, run
  `ajx console start <trial> --plan <id> --quote "<their words>"`. Use the plan ID from `since`;
  its first 7 or more characters are enough. The command checks readiness, records the quote and
  launches. It refuses a plan that changed after the user saw it.
- **Watch only when asked.** Loop `ajx console wait <trial> --cursor N --timeout 90`, passing back
  the `cursor` each call returns. It returns when the journal gains an entry (`woke: journal`) or
  at the timeout (`woke: timeout`). Entries include the user's actions and run milestones from
  actor `runner`, such as "the worker finished", "verification succeeded" and "report ready".
  Tell the user the chat is paused while you wait, and keep the timeout under your tool-call
  limit.
- **Respect what changed in the browser.** If the plan's approval is `stale` or the user edited
  the trial, re-read the files before you change anything. Follow `do_not`.
- **Deliver results as usual.** A run started in the console needs no `ajx run` from you. When
  `since` shows it finished, explain the outcome as in step 4 of the skill. Link the page's
  Results tab (`console.results_url`) and the report files (`runs[].files`).

`since` returns:

| Field | Meaning |
|---|---|
| `cursor`, `since` | Journal position now and at your previous call |
| `headline` | One line about the trial's current state |
| `changes` | Journal entries after your cursor: `actor` (user, agent, console, runner), `kind`, readable `text` |
| `messages_for_agent` | The user's messages you have not answered with a `--reply-to` note |
| `plan` | The plan ID, whether it can be fingerprinted, and its approval: `none`, `valid` or `stale`, `via` console or chat |
| `readiness` | The last check's status, whether it covers the current plan, and its problems |
| `process`, `runs`, `output` | What is running; each attempt's stage, verification, cleanup, asks and report `files`; the output folder |
| `console` | Whether the server is running, and its `url` and `results_url` |
| `do_not` | Actions to avoid in the current state |

## Files

The console keeps its state in `<trial-dir>/.ajx-console/`:

- `actions.jsonl`: the journal
- `readiness.json`: the last readiness check
- `agent.json`: the agent's last check-in
- `server.json`: the port and the page key, mode 0600
- `logs/`: output of the commands the console started

None of this is given to the worker. Keep the folder out of fixture and mount directories, and
out of version control; the repository's `.gitignore` excludes it.

## Limits

- One console per trial directory, on the local machine. For a remote developer machine, forward
  the port over SSH.
- The page edits the time limit, attempts, budget and models directly, and the task prompt until
  attempts exist. Everything else needs the `trial.toml` editor or your agent.
- Where the worker runs is a trial-file change. A container needs a prepared image and explicit
  credentials; see [environments.md](environments.md).
- The console assumes the person and the agent using it are the same user who started it.
  It refuses other hosts and requests without the page key, and it serves reports as sandboxed
  pages. It does not defend against other processes running as that user.
