package preview

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
)

type connectionBook struct {
	Default string                `json:"default"`
	Hosts   map[string]Connection `json:"connections"`
}

func (client Client) connectionBook() (connectionBook, error) {
	book := connectionBook{Hosts: map[string]Connection{}}
	previous := filepath.Join(client.legacyState(), "connections.json")
	var legacy connectionBook
	legacyErr := readJSON(previous, &legacy)
	if legacyErr == nil {
		book.Default = legacy.Default
		maps.Copy(book.Hosts, legacy.Hosts)
	}
	var preview Connection
	if err := readJSON(client.connectionPath(), &preview); err == nil {
		book.Default = preview.Target
		book.Hosts[preview.Target] = preview
	} else if !errors.Is(err, os.ErrNotExist) {
		return book, err
	}
	var native connectionBook
	if err := readJSON(client.bookPath(), &native); err == nil {
		book.Default = native.Default
		maps.Copy(book.Hosts, native.Hosts)
	} else if !errors.Is(err, os.ErrNotExist) {
		return book, err
	}
	return book, nil
}

func (client Client) bookPath() string {
	return filepath.Join(client.state, "connections.json")
}

func (client Client) saveConnection(label string, connection Connection) error {
	return withLock(filepath.Join(client.state, "connections.lock"), func() error {
		book, err := client.connectionBook()
		if err != nil {
			return err
		}
		if previous, exists := book.Hosts[label]; exists && previous.Target != connection.Target {
			return fmt.Errorf("connection %q points to another server. Choose another --name", label)
		}
		book.Hosts[label] = connection
		book.Default = label
		return writeJSON(client.bookPath(), book)
	})
}

func (client Client) resolveConnection(label string) (Connection, error) {
	book, err := client.connectionBook()
	if err != nil {
		return Connection{}, err
	}
	if label == "" {
		label = book.Default
	}
	if connection, exists := book.Hosts[label]; exists {
		return connection, nil
	}
	if label != "" {
		return newConnection(label, "")
	}
	return Connection{}, errors.New("connect a Linux server with remote-dev connect user@server")
}

func (client Client) legacyState() string {
	return filepath.Join(filepath.Dir(client.state), "standalone-v1")
}

func (client Client) legacyCache() string {
	return filepath.Join(filepath.Dir(client.cache), "standalone-v1")
}
