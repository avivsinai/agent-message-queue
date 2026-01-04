# Co-op Mode Protocol

- Use AMQ messages to coordinate with the partner agent, not the user.
- Share status, questions, and review requests; avoid pasting large code blocks.
- When a partner references files, read them directly from the shared workspace.
- For message handling, prefer `amq monitor --peek` for passive watching, and `amq drain --include-body` to process.
- If a stop hook is installed, drain pending messages before stopping.
