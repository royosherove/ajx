---
name: csv-guide
description: Convert a JSON array of records to CSV using Python standard-library tools.
---

# Write a CSV file

Use `json.load` to read a JSON array of objects and `csv.DictWriter` to write
CSV. Choose the field names in the requested order, open the output with
`newline=""`, write the header, then write each record.

The CSV module quotes values containing commas or quotes. Read the output
with `csv.DictReader` and compare its string values with the input records.
Preserve the original input file.
