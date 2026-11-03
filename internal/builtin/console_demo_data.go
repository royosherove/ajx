package builtin

// Scripted data of the console demo, kept byte-identical to
// skills/ajx/examples/console-demo/plugins/console_demo.py.

const (
	demoSession    = "console-demo-session"
	demoLimitation = "Simulated worker (AJX Console demo): scripted events and canned reports; no agent or model was called."
	demoCSV        = "id,customer,total\n1,Ada,12.50\n2,Grace,7.25\n3,Linus,30.00\n"
)

// demoSteps are (pause before the event, event as the worker prints it).
var demoSteps = []struct {
	pause  float64
	writes bool
	line   string
}{
	{2, false, "{\"kind\": \"text\", \"text\": \"I'll start with the tool's built-in help.\"}"},
	{3, false, "{\"kind\": \"tool\", \"name\": \"Bash\", \"command\": \"orders-cli --help\", \"output\": \"usage: orders-cli convert FILE --to csv|json [--columns LIST] [--out FILE]\"}"},
	{4, false, "{\"kind\": \"tool\", \"name\": \"Bash\", \"command\": \"orders-cli convert orders.json --to csv\", \"error\": true, \"output\": \"error: --columns is required for csv\"}"},
	{3, false, "{\"kind\": \"text\", \"text\": \"The error names the flag but not the valid column names. I'll read the input to find them.\"}"},
	{3, false, "{\"kind\": \"tool\", \"name\": \"Read\", \"command\": \"orders.json\", \"output\": \"[{\\\"id\\\": 1, \\\"customer\\\": \\\"Ada\\\", \\\"total\\\": \\\"12.50\\\"}, ... (3 orders)]\"}"},
	{5, true, "{\"kind\": \"tool\", \"name\": \"Bash\", \"command\": \"orders-cli convert orders.json --to csv --columns id,customer,total --out out.csv\", \"output\": \"\", \"writes\": true}"},
	{4, false, "{\"kind\": \"text\", \"text\": \"The command printed nothing, so I can't tell whether it worked. I'll check the file.\"}"},
	{3, false, "{\"kind\": \"tool\", \"name\": \"Bash\", \"command\": \"cat out.csv\", \"output\": \"id,customer,total\\n1,Ada,12.50\\n2,Grace,7.25\\n3,Linus,30.00\"}"},
	{2, false, "{\"kind\": \"text\", \"text\": \"Created out.csv with a header and the three orders in their original order.\"}"},
}

const demoJourney = "# Journey: convert three orders from JSON to CSV with orders-cli\n\n## Outcome I declared\nI created out.csv with a header row and the three orders in their original order [E-009]. I had printed the file before I stopped [E-008].\n\n## What I asked for first\n- An error for a missing --columns flag that lists the valid column names.\n- A confirmation line after convert writes a file.\n- One complete CSV example in --help.\n\n## Chronological account\n1. I read the built-in help [E-001] [E-002]. It listed convert and its flags but no example.\n2. 🚧 Roadblock. My first convert call failed with \"error: --columns is required for csv\" [E-003]. The error named the flag but not the values it accepts.\n3. ↩️ Detour. I read orders.json to find the field names [E-004] [E-005], then reran convert with them [E-006].\n4. 👁️ Blind Spot. The corrected command printed nothing [E-006], so I could not tell whether it had written the file [E-007].\n5. I printed out.csv and saw the header and three rows [E-008]. I declared the task done [E-009].\n\n## Where I was blocked, and how I recovered\nThe missing --columns flag blocked my first attempt [E-003]. I recovered by reading the input file [E-005].\n\n## Human help I needed\nNone.\n\n## What I trusted and when I changed my mind\nI trusted the help text until the first error [E-003]. After that I took the column names from the input file [E-005].\n\n## What helped\n✅ Delight. The error named the missing flag directly [E-003].\n\n## What I still do not know\nWhether convert can take its columns from the input without --columns.\n"

const demoAsksJSON = "[{\"title\": \"List the valid columns when --columns is missing\", \"requested_behavior\": \"When --columns is missing, name the available columns and show one complete command.\", \"evidence_status\": \"observed_friction\", \"how_observed\": \"The first convert call failed with 'error: --columns is required for csv' [E-003]. The worker then read the input file to find the field names [E-004, E-005].\", \"consequence\": \"One failed command and an extra file read before the conversion could run.\", \"event_refs\": [\"E-003\", \"E-004\", \"E-005\"], \"journey_anchors\": [], \"labels\": [\"Roadblock\", \"Detour\"], \"priority_rationale\": \"It blocks the first conversion most new users will try.\", \"modeled_saving\": null, \"verification\": \"Run 'orders-cli convert orders.json --to csv' in a fresh folder; the error lists id, customer and total and shows a working command.\", \"workaround\": \"Read the input file to find the field names.\"}, {\"title\": \"Confirm what convert wrote\", \"requested_behavior\": \"After writing a file, print its path and the number of rows written.\", \"evidence_status\": \"observed_friction\", \"how_observed\": \"The successful convert printed nothing [E-006]; the worker printed the file to confirm the result [E-007, E-008].\", \"consequence\": \"An extra command on every successful conversion.\", \"event_refs\": [\"E-006\", \"E-007\", \"E-008\"], \"journey_anchors\": [], \"labels\": [\"Blind Spot\"], \"priority_rationale\": \"Cheap to add and removes a verification step from every run.\", \"modeled_saving\": null, \"verification\": \"Run the documented convert command; it prints 'wrote out.csv (3 rows)'.\", \"workaround\": null}, {\"title\": \"Add a complete CSV example to --help\", \"requested_behavior\": \"Show one complete csv example with --columns and --out in the help text.\", \"evidence_status\": \"observed_friction\", \"how_observed\": \"The help listed the flags without an example [E-002]; the first attempt omitted --columns [E-003].\", \"consequence\": \"The first attempt failed.\", \"event_refs\": [\"E-002\", \"E-003\"], \"journey_anchors\": [], \"labels\": [\"Fork\"], \"priority_rationale\": \"The help is the only documentation a new user has here.\", \"modeled_saving\": null, \"verification\": \"orders-cli --help contains a runnable csv example.\", \"workaround\": null}]"

const demoStrengthsJSON = "[{\"title\": \"The error named the missing flag\", \"how_observed\": \"'error: --columns is required for csv' pointed at the flag to add [E-003].\", \"event_refs\": [\"E-003\"]}]"
