# Review, start and watch a trial in the AJX Console

`ajx console` serves a local web page for one trial. You can review the plan, set the common
parameters, check readiness, start, stop, watch the run and read the results there. Your agent
sees the same trial from chat. The console runs as the user who started it, on `127.0.0.1` only.

## Try it without an agent or a model

```sh
ajx console demo trials/console-demo
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
| Plan | See the whole trial as a map and edit any part of it (see [The Plan editor](#the-plan-editor)). The **Best result** checklist reviews the setup. Edit `trial.toml` directly under Advanced; an edit is saved only if AJX can load it. Check readiness, then start, resume, finish a report, or run again. |
| Live | Watch every attempt on the same map as the Plan tab, with a pulse on what each one is doing now (see [The Live tab](#the-live-tab)). Each attempt shows its stages, model, the worker's latest events and any error it stopped with, the files in its workspace, verification, cleanup and reporting, plus AJX's own output. Stop or restart a run, or retry cleanup when it was not confirmed. |
| Results | Read each attempt's verified outcome first, then its journey as a timeline of labeled steps, then the asks, helpful behavior and gates (see [The Results tab](#the-results-tab)). With more than one attempt, a table compares them one row per attempt. Open the report, the journey and `asks.md`. Export a run for a team, or rebuild the reports without model calls. |
| Activity | See everything you, your agent, the console and the runner did, as one story. Send a message to your agent and see whether it answered. |

Each button runs an existing command, and the page shows it: `ajx validate`, `ajx doctor`,
`ajx run`, `ajx run --cells C --stages S`, `ajx report --no-synthesis` and `ajx export`. The page
adds no other way to execute a task.

Live worker events need an adapter that reports progress. Claude Code and the demo do. Other
agents show stages only until the report lists every event.

## The Plan editor

The Plan tab shows the trial as a map in three layers:

- **Your side:** the task, the success checks, the run settings, the reporter, the task account,
  and the output folder under Advanced. These run on your machine. Checks and the reporter run
  after the worker stops.
- **Execution environments:** where the worker runs. A fresh folder or a fresh profile on your
  machine is marked **Not a sandbox**: the worker runs as you. A container box reads **Runs in a
  container**. It sees only its own folders, but it still has the network and the credentials
  you give it.
- **Workers:** one card for each agent setup, inside the environment it runs in. Drag a card to
  another environment, or use **Runs in** in its editor.

Click a box to edit it in place. The map updates as you type. Press Esc or ✕ to close the editor.
A console link ending in `#open=task` (or `checks`, `run`, `reporter`, `account`, `advanced`,
`worker-<name>`, `env-<name>`) opens that editor when the page loads.

- **Best result** lists ten checks for a trustworthy setup, such as a success check of the actual
  outcome, a neutral prompt, a clean baseline worker, enough attempts to see variance, a strong
  reporter, comparisons that change one thing, and preflight and cleanup for a task account. Each
  item that needs attention has **Show me**, and many have a one-click **Fix**. This is advice
  about setup quality, not a guarantee of a good result. Only real errors block saving.
- **Recipes** change several settings at once: a first-time-user baseline, a comparison of two
  models, with versus without a skill, and a cloud task run safely. Open one to see each change
  and the boxes it touches before you press **Apply**.
- **Unsaved changes** collect in the bar at the bottom. Use **Save**, **Discard** or **Undo**.
  **What this writes** shows the exact `trial.toml` and prompt diff, or the whole file. Start and
  Run again wait until you save or discard, so what you approve is what is saved.
  The editor is read only for the moment a save takes.
- A new sign-in for a container or fresh profile, such as Bedrock keys or a Kiro API key, adds an
  `[auth.*]` table. It holds only `${VAR}` references, which are read from the terminal that
  started the console. No secret is written.
- If your agent or an editor changes something the editor shows while you have unsaved edits, the
  bar says so. **Reload** shows the new version and drops your edits. A change the editor does
  not show, such as a new comment, keeps your edits.
- Saving rewrites only the sections you changed. Other keys, sections and comments stay as written.
  Settings the editor does not show, such as directory mounts, are kept as written. Change those
  in `trial.toml` or ask your agent.

## The Live tab

The Live tab draws the environment and worker boxes of the Plan map. Each worker card lists its
attempts with a bar of their stages, and a pulse marks the stage running now. The boxes on your side
light up while an attempt is being prepared, checked or reported, and each environment box says how
its cleanup went. Select an attempt to jump to its card. A stage that was still running when AJX
stopped unexpectedly reads **AJX is not running** instead of pulsing.

Each attempt's card shows everything it produced so far:

- **Worker feed:** the worker's latest events. The report lists every event.
- **Files in its workspace:** names, sizes and times, including the starting files. **New or
  changed** means modified after the worker started. Select a name to read it on the page as plain
  text, or open it in a new tab. A file that is not text downloads. Nothing is run. The list shows
  up to 200 files. A long file shows its beginning, and the console serves up to 2 MB of any file.
- **Verification:** each success check with its result and details. Checks run on your machine after
  the worker stops, unless a check is marked to run inside the environment.
- **Cleanup:** task cleanup, then the environment. A fresh folder moves into the run's evidence, a
  fresh profile is released, and a container is removed. Anything AJX could not confirm reads
  **not confirmed**, with a button to retry.
- **Reporting:** the transcript, the journey (the worker's own account, or a labeled reconstruction
  and why), the asks, and links to the report. The reporter runs on your machine after the worker
  stops. If the analysis failed, **Re-run the analysis** asks before it calls the reporter model.

When a run ends with a new report, the page opens Results. After a Stop it stays on Live, so you can
check the cleanup.

## The Results tab

Each attempt's card starts with its **verified outcome**: whether the success checks passed, each
check by name, and where they ran. A check that hit credential errors, or a worker that could not
start, means the checks do not show what the worker achieved: the attempt reads **Not verified**
with the reason, whatever the checks returned. What the worker said when it stopped is shown below
as its own claim; the checks are what AJX verified.

- **Journey:** the numbered steps of the journey's chronological account, in order. A step with a
  label shows its icon (Roadblock, Misdirection, Blind Spot and the rest; hover one for its
  meaning), and above the steps a count for each label. Each event id, such as `E-003`, opens the
  journey page at that event. The page shows up to 30 steps and says how many there are in all;
  the counts cover every step.
- **Who wrote it:** the worker's own account, given when AJX resumed it after the task, or a
  **labeled reconstruction** that the reporter wrote from the recorded evidence. A reconstruction
  is never shown as the worker's recollection, and the card says why there is one: the model's
  safeguards stopped the worker's own account, the worker's reply did not contain the requested
  `# Journey` account, the worker's reply was empty or too short, or the worker could not be
  resumed. Its labels are the reporter's.
- **Asks, what helped and gates:** as in the report, each linked to its evidence. Asks are the
  reporter model's judgments of one attempt, not a score. An attempt with no asks did not
  necessarily go smoothly; check its verification and journey.

With two or more attempts, **Compare the attempts** lists one row per attempt, grouped by worker
and model: the verified outcome, who wrote the journey, the count of each label, and the number of
asks. A label cell stays empty when no steps could be read from that attempt's journey. Select an
attempt to jump to its card. The matrix overview (`index.html`) compares them in the report.

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
  output folder with attempts. You can still change the number of attempts and the output folder,
  and add workers to the existing environments. To change anything else, switch to a fresh output
  folder first. While a run or command is in progress, the Plan tab is read only.
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
sets the AWS profile for the worker's tool shell, set `shell_startup = "skip"` in `[trial]`,
then rerun doctor and review the effective identity and remaining warnings.
Skipping shell startup does not disable harness settings or establish a
filesystem boundary; see [configuration inheritance](briefing.md#configuration-inheritance).

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
- The Plan editor covers what a test runner usually sets. Everything else needs the `trial.toml`
  editor or your agent.
- A container needs an image already on this machine, pinned by digest. It also needs a sign-in
  that works with a fresh home folder: an API key or cloud keys from your terminal, not a desktop
  login. See [environments.md](environments.md).
- The console assumes the person and the agent using it are the same user who started it.
  It refuses other hosts and requests without the page key, and it serves reports as sandboxed
  pages. It does not defend against other processes running as that user.
