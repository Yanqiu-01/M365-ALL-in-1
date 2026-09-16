package chathub

// FileEditProtocolNote is shared by native/plugin, fenced and router prompts.
// JSON spelling and decoded string values are distinct: asking for bare control
// bytes in a JSON string would make every such call depend on salvage.
// Keep the wording affirmative, as with environmentPrompt.
const FileEditProtocolNote = `Tool argument strings use standard JSON escaping. In the JSON text, \t represents a tab and \n represents a newline in the decoded value; \\t and \\n represent literal backslash-letter text and are appropriate only when those literal characters occur in the file. For example, {"old_string":"\tfirst\n\tsecond"} describes two tab-indented lines.
For a declared Read/Edit tool, the Read listing's line number and its single delimiter (→, or one TAB in older listings) are display metadata. The file content starts immediately after that delimiter, including every remaining tab and space. Build old_string from that content in the latest successful read of the target range.
After an Edit fails or the target's current contents are uncertain, refresh the relevant range with the declared reading tool before preparing another Edit. A successful edit changes the file, so subsequent old_string values refer to the updated contents.`
