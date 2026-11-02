# AJX Console demo

A trial whose worker is simulated by `plugins/console_demo.py`. It prints a scripted session for
a fictional `orders-cli`, writes `out.csv`, and answers the narration and reporting calls with
canned text. No agent, model or network is used.

```sh
python3 skills/ajx/bin/ajx console demo trials/console-demo
```

That copies this folder to `trials/console-demo` and opens the console. Press **Check and start**
to watch the live feed, verification, cleanup and reporting. A run takes about a minute; set
`AJX_DEMO_PACE=0.2` before opening the console to speed it up.
