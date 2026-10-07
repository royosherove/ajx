# Synthetic harness fixtures

These JSONL files model the event shapes consumed by the Claude Code and Kiro
adapters. They are synthetic examples, not recordings of a person's agent
session. Session and message IDs, timestamps, paths, usage values, and metering
values are test data.

The Claude pair deliberately repeats one assistant message ID across thinking
and tool-use blocks. The transcript has finalized output counts of 123 and 8;
the stream's total is 131. This exercises deduplication and reconciliation,
including the difference between provisional stream usage and final usage.
The Kiro fixture covers tool calls, streamed text chunks, metering, and completion.

Keep private environment metadata, installed-tool descriptions, system prompts,
signatures, provider request IDs, and real session data out of these files.
