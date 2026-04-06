// Package logger provides a thread-safe, rotating file logger with UTC timestamps.
// Connection and error logs are written to separate date-stamped files.
// Old log files are automatically purged based on retention configuration.
package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Level represents log severity.
type Level int

const (
	DEBUG Level = iota
	INFO
	WARN
	ERROR
)

func (l Level) String() string {
	switch l {
	case DEBUG:
		return "DEBUG"
	case INFO:
		return "INFO "
	case WARN:
		return "WARN "
	case ERROR:
		return "ERROR"
	default:
		return "?????"
	}
}

// Logger writes to rotating daily log files and optionally to stdout.
// Two separate files are maintained: one for connection events and one for errors.
type Logger struct {
	mu sync.Mutex

	logDir  string
	prefix  string
	verbose bool // also print to stdout

	connRetainDays int // days to keep connection logs
	errRetainDays  int // days to keep error logs

	connFile *os.File // current connection log file
	errFile  *os.File // current error log file
	connDate string   // YYYY-MM-DD of current conn file
	errDate  string   // YYYY-MM-DD of current err file
}

// New creates a new Logger. logDir is created if it does not exist.
// connRetainDays/errRetainDays set how long log files are kept (0 = forever).
func New(logDir, prefix string, connRetainDays, errRetainDays int, verbose bool) (*Logger, error) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("logger: failed to create log directory %q: %w", logDir, err)
	}

	l := &Logger{
		logDir:         logDir,
		prefix:         prefix,
		verbose:        verbose,
		connRetainDays: connRetainDays,
		errRetainDays:  errRetainDays,
	}

	// Open initial log files.
	if err := l.rotateIfNeeded(); err != nil {
		return nil, err
	}

	// Run retention cleanup on startup, then daily.
	l.purgeOldLogs()
	go l.dailyMaintenance()

	return l, nil
}

// ─── Public log methods ────────────────────────────────────────────────────

// Conn logs a connection-lifecycle event (INFO level) to the connection log.
func (l *Logger) Conn(format string, args ...any) {
	l.write(l.connFile, INFO, format, args...)
}

// ConnError logs an error to BOTH the connection log and the error log.
func (l *Logger) ConnError(format string, args ...any) {
	l.write(l.connFile, ERROR, format, args...)
	l.write(l.errFile, ERROR, format, args...)
}

// Info logs an informational message to the connection log (and stdout if verbose).
func (l *Logger) Info(format string, args ...any) {
	l.write(l.connFile, INFO, format, args...)
}

// Warn logs a warning to both logs.
func (l *Logger) Warn(format string, args ...any) {
	l.write(l.connFile, WARN, format, args...)
	l.write(l.errFile, WARN, format, args...)
}

// Error logs an error to the error log (and stdout always).
func (l *Logger) Error(format string, args ...any) {
	l.write(l.errFile, ERROR, format, args...)
	// Errors are always printed to stdout regardless of verbose setting.
	if !l.verbose {
		ts := time.Now().UTC().Format("2006-01-02 15:04:05.000 UTC")
		fmt.Fprintf(os.Stderr, "[%s] [%s] %s\n", ts, ERROR, fmt.Sprintf(format, args...))
	}
}

// Debug logs a debug message (only if verbose=true).
func (l *Logger) Debug(format string, args ...any) {
	if l.verbose {
		l.write(l.connFile, DEBUG, format, args...)
	}
}

// ─── Internal helpers ─────────────────────────────────────────────────────

func (l *Logger) write(f *os.File, level Level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Rotate if the date has changed.
	_ = l.rotateIfNeeded()

	ts := time.Now().UTC().Format("2006-01-02 15:04:05.000 UTC")
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("[%s] [%s] %s\n", ts, level, msg)

	if f != nil {
		_, _ = f.WriteString(line)
	}

	// Print to stdout when verbose or for errors.
	if l.verbose || level >= ERROR {
		fmt.Print(line)
	}
}

// rotateIfNeeded checks if the current log files are from today; if not, opens new ones.
// Must be called with l.mu held.
func (l *Logger) rotateIfNeeded() error {
	today := time.Now().UTC().Format("2006-01-02")

	if l.connFile == nil || l.connDate != today {
		if err := l.openConnLog(today); err != nil {
			return err
		}
		l.connDate = today
	}

	if l.errFile == nil || l.errDate != today {
		if err := l.openErrLog(today); err != nil {
			return err
		}
		l.errDate = today
	}

	return nil
}

func (l *Logger) openConnLog(date string) error {
	if l.connFile != nil {
		_ = l.connFile.Close()
	}
	name := filepath.Join(l.logDir, fmt.Sprintf("%s_connections_%s.log", l.prefix, date))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("logger: failed to open connection log %q: %w", name, err)
	}
	l.connFile = f
	return nil
}

func (l *Logger) openErrLog(date string) error {
	if l.errFile != nil {
		_ = l.errFile.Close()
	}
	name := filepath.Join(l.logDir, fmt.Sprintf("%s_errors_%s.log", l.prefix, date))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("logger: failed to open error log %q: %w", name, err)
	}
	l.errFile = f
	return nil
}

// purgeOldLogs deletes log files older than their configured retention period.
func (l *Logger) purgeOldLogs() {
	now := time.Now().UTC()

	entries, err := os.ReadDir(l.logDir)
	if err != nil {
		return
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), l.prefix+"_") {
			continue
		}

		// Extract date from filename: prefix_type_YYYY-MM-DD.log
		base := strings.TrimSuffix(e.Name(), ".log")
		parts := strings.Split(base, "_")
		if len(parts) < 1 {
			continue
		}
		dateStr := parts[len(parts)-1]
		fileDate, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}

		age := int(now.Sub(fileDate).Hours() / 24)

		isConn := strings.Contains(e.Name(), "_connections_")
		isErr := strings.Contains(e.Name(), "_errors_")

		expired := false
		if isConn && l.connRetainDays > 0 && age > l.connRetainDays {
			expired = true
		}
		if isErr && l.errRetainDays > 0 && age > l.errRetainDays {
			expired = true
		}

		if expired {
			path := filepath.Join(l.logDir, e.Name())
			if err := os.Remove(path); err == nil {
				fmt.Printf("[logger] Purged expired log file: %s (age: %d days)\n", e.Name(), age)
			}
		}
	}
}

// dailyMaintenance runs log rotation and purging once per day at midnight UTC.
func (l *Logger) dailyMaintenance() {
	for {
		// Sleep until next UTC midnight.
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 30, 0, time.UTC)
		time.Sleep(time.Until(next))

		l.mu.Lock()
		_ = l.rotateIfNeeded()
		l.mu.Unlock()

		l.purgeOldLogs()
	}
}

// Close flushes and closes all open log files.
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.connFile != nil {
		_ = l.connFile.Close()
		l.connFile = nil
	}
	if l.errFile != nil {
		_ = l.errFile.Close()
		l.errFile = nil
	}
}
