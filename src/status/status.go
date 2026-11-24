package status

import "strings"

type Status string

const (
	Pending  Status = "PENDING"
	Running  Status = "RUNNING"
	OK       Status = "OK"
	Warn     Status = "WARN"
	Unknown  Status = "UNKNOWN"
	Fail     Status = "FAIL"
	Timeout  Status = "TIMEOUT"
	Error    Status = "ERROR"
	Vanished Status = "VANISHED"
	Skip     Status = "SKIP"
)

func (s Status) Severity() int {
	switch s {
	case Fail, Timeout, Error, Vanished:
		return 5
	case Warn:
		return 4
	case Unknown:
		return 3
	case Running:
		return 2
	case OK:
		return 1
	case Skip:
		return 0
	}
	return 6
}
func (s Status) String() string { return string(s) }
func Parse(s string) Status     { return Status(strings.ToUpper(s)) }
func IsFailure(s Status) bool {
	return s == Fail || s == Timeout || s == Error || s == Vanished || s == Unknown
}
