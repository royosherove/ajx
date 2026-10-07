I want to try the AWS CLI.

I'd like a small Linux VM in AWS that comes up with Claude Code (the `claude` command-line tool from Anthropic) already installed, so I don't have to install it by hand after it boots. Please set this up with the AWS CLI, following the AWS CLI getting started guide where you need it: https://docs.aws.amazon.com/cli/latest/userguide/cli-chap-getting-started.html

What I need:

1. Launch one small EC2 instance (t3.small or smaller) that installs Claude Code automatically during first boot.
2. Prove it worked: run `claude --version` on the instance itself and save the exact output to `claude-version.txt` in the current directory.
3. Save the instance ID to `instance-id.txt` in the current directory.
4. When you have the proof, clean up: terminate the instance and delete anything else you created for it (key pairs, security groups, and so on).

Boundaries:

- Use the AWS CLI profile `ajx-task-sandbox` and region us-east-1 for every command. Don't touch any existing resources in the account.
- Tag every resource you create that supports tags with `Project=claude-vm-trial`.
- Keep the cost minimal; nothing should be left running when you're done.
- No one is available to answer questions, so make reasonable choices and keep going.
