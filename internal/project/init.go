package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var modeTemplates = map[string]string{
	"compose": `
# Each forwarded service must publish its target port on 127.0.0.1 in Compose.
compose_file = "compose.yaml"

# [forwards.web]
# service = "web"
# target = 8000
# local = "auto"
`,
	"commands": `
# Commands run inside the selected image with the synced project mounted at /workspace.
[services.web]
image = "python:3.13-slim"
command = ["python", "-m", "http.server", "8000", "--bind", "0.0.0.0"]

[forwards.web]
service = "web"
target = 8000
local = "auto"
`,
	"node": `
# Each directory needs a package.json with the chosen npm script.
# The script must listen on 0.0.0.0 and the matching target port inside Docker.
[services.frontend]
directory = "frontend"
script = "dev"

[services.backend]
directory = "backend"
script = "dev"

[forwards.frontend]
service = "frontend"
target = 3000
local = "auto"

[forwards.backend]
service = "backend"
target = 4000
local = "auto"
`,
}

func Init(root, host, mode string) (string, error) {
	if _, err := validateName(host, "SSH host alias"); err != nil {
		return "", err
	}
	if _, err := validateMode(mode); err != nil {
		return "", err
	}
	filename := filepath.Join(root, ConfigFile)
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	_, writeErr := fmt.Fprintf(file, "host = %q\nmode = %q\n%s", host, mode, modeTemplates[mode])
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return "", err
	}
	return filename, nil
}
