package tools

import "time"

func unixMilliRFC3339(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
