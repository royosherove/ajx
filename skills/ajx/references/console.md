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
| Plan | Read the task prompt the worker receives and the success checks. See where the worker runs and how it signs in. Set the time limit, attempts, budget and models; the model field suggests names the agent accepts. Edit `trial.toml` directly; an edit is saved only if AJX can load it. Check readiness, then start, resume, finish a report, or run again. |
| Live | Follow each attempt's stages, model, the worker's latest events and any error it stopped with, verification, cleanup and reporting, plus AJX's own output. Stop or restart a run, or retry cleanup when it was not confirmed. |
| Results | Read the verified outcome and the asks, with an icon for each label (Roadblock, Misdirection, Blind Spot and the rest; hover one for its meaning), helpful behavior and gates. Open the report, the journey and `asks.md`. Export a run for a team, or rebuild the reports without model calls. |
| Activity | See everything you, your agent, the console and the runner did, as one story. Send a message to your agent and see whether it answered. |

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

## Output folders, running again and stopping

- **Resume and finish a report.** Start resumes an output folder with unfinished work. Its label
  says what it does: **Resume: N attempt(s) left**, or **Finish the report for r1** when the
  worker already stopped. It never runs a finished or stopped attempt again.
- **Run again.** Once attempts exist, **Run again here** adds one more attempt in the same folder
  with the same settings, so the results compare directly. **Run again in a fresh folder** points
  `trial.output_dir` at a new, empty folder. Both check readiness and start.
- **Settings are locked once attempts exist.** The task prompt, models, time limit, budget and
  reporter stay as the attempts used them, and `ajx run` refuses a changed configuration for an
  output folder with attempts. To change them, switch to a fresh output folder first.
- **Stop.** Stop sends SIGINT to `ajx run`. AJX stops the worker, runs task cleanup, releases the
  environment and archives the evidence. When the worker already finished, the button reads
  **Stop reporting**: stopping then leaves the report for **Finish the report**. A second Stop is
  refused while cleanup runs. `ajx run` treats SIGTERM and SIGHUP the same way. The page cannot
  stop a run with `parallel` above 1, because AJX would wait for every running attempt.
- **Restart.** While a run is in progress, **Restart** stops it the same way, waits for the
  cleanup, then runs again here or in a fresh folder.
- **A worker that could not start.** When the worker exits with an error within two minutes
  without a single tool call, for example because the agent rejected the model name, the page,
  the journal and `since` say so and quote the error. Fix the cause, then use Run again.

## Accounts and readiness warnings

When the task acts in a cloud or SaaS account (it declares `identity_cmd` or `[[preflight]]`),
warnings from `ajx doctor` must be accepted before starting: tick **I accept these warnings for
this run**, and the approval records them. If doctor says a startup file such as `~/.zshenv`
sets the AWS profile for the worker's tool shell, set `shell_startup = "skip"` in `[trial]`
instead of accepting the risk.

## Stop the console

**Stop console** in the page header stops the server for that trial; runs keep going. From a
terminal:

```sh
ajx console list                 # every console server running for you, with its link
ajx console stop trials/my-cli   # one trial's console
ajx console stop --all           # every console (same as: ajx console stop all)
```

## For the coordinating agent

The console is the default way to hand a trial to a user who is present. Open it after the
trial validates. Draft the task prompt, the success check and any fixture as usual; the page
does not write task text. Skip it only for unattended runs or when the user asks to stay in chat.

```sh
ajx console <trial-dir> --no-open      # prints the link; the server keeps running
```

Tell the user what needs their decision, and that you cannot see the page live. Then:

- **Catch up before every reply.** Run `ajx console since <trial> --cursor N`, passing the
  `cursor` from your previous call, or 0 the first time. Summarize `headline` and `changes` in a
  sentence or two; never paste the JSON. Answer each item in `messages_for_agent`, and when the
  user is in the browser, post a short answer with `ajx console note <trial> "..." --reply-to <n>`.
- **Never approve, start, stop, restart or run again for the user.** If they approve in chat, run
  `ajx console start <trial> --plan <id> --quote "<their words>"`. Use the plan ID from `since`;
  its first 7 or more characters are enough. The command checks readiness, records the quote and
  launches. It refuses a plan that changed after the user saw it.
- **Run a finished trial again when asked.** Use `ajx console again <trial> --same-folder` for one
  more comparable attempt, or `--fresh-folder` after changed settings, with `--plan` and `--quote`.
  Never edit `repetitions` or `output_dir` by hand for this.
- **Readiness warnings.** When `since` lists `readiness.warnings` with
  `warnings_need_acceptance`, show them to the user. Pass `--accept-warnings` only when they
  accepted them in their own words.
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
| `process`, `runs`, `output` | What is running; each attempt's stage, model, `exit_code`, `stop_reason`, `error` and `could_not_start`, verification, cleanup, asks and report `files`; the output folder, its `pending_reports` and `unstarted` attempts |
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
