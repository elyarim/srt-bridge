package main

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Operating system details change much less often than stream statistics.
type statusCache[T any] struct {
	mu    sync.Mutex
	key   string
	until time.Time
	value T
}

func (c *statusCache[T]) get(key string, read func() T) T {
	c.mu.Lock()
	defer c.mu.Unlock()
	if key != c.key || time.Now().After(c.until) {
		c.value = read()
		c.key = key
		c.until = time.Now().Add(10 * time.Second)
	}
	return c.value
}

func (c *statusCache[T]) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until = time.Time{}
}

type programLocation struct {
	path  string
	found bool
}

func findProgram(custom, component, project, exe string) programLocation {
	if custom != "" {
		_, err := os.Stat(custom)
		return programLocation{custom, err == nil}
	}
	dir := exeDir()
	for _, path := range []string{
		filepath.Join(dir, component, exe),
		filepath.Join(dir, "..", project, "bin", exe),
	} {
		if _, err := os.Stat(path); err == nil {
			return programLocation{path, true}
		}
	}
	return programLocation{}
}
