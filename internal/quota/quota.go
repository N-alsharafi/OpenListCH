package quota

import (
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/go-cache"
)

// QuotaData stores download timestamps for a connection
type QuotaData struct {
	Timestamps []time.Time
}

// quotaCache stores download quotas with 24h TTL
var quotaCache = cache.NewMemCache[*QuotaData](cache.WithShards[*QuotaData](16))

// Default values
const (
	DefaultLimit        = 10
	DefaultWindowHours  = 24
)

// GetConnectionID generates a unique identifier for the connection
// based on IP address and browser fingerprint
func GetConnectionID(ip, userAgent, acceptLang string) string {
	data := ip + "|" + userAgent + "|" + acceptLang
	hash := utils.HashData(utils.SHA256, []byte(data))
	return hash[:16] // Return first 16 characters
}

// CheckRemaining returns the number of remaining downloads allowed
// for a connection within the sliding window
func CheckRemaining(connID string, limit int) (int, error) {
	if limit <= 0 {
		limit = DefaultLimit
	}

	cutoff := time.Now().Add(-time.Duration(DefaultWindowHours) * time.Hour)

	data, exists := quotaCache.Get(connID)
	if !exists {
		return limit, nil
	}

	// Filter timestamps to only include those within the window
	valid := make([]time.Time, 0, len(data.Timestamps))
	for _, ts := range data.Timestamps {
		if ts.After(cutoff) {
			valid = append(valid, ts)
		}
	}

	// Update cache with filtered timestamps
	if len(valid) > 0 {
		quotaCache.Set(connID, &QuotaData{Timestamps: valid}, cache.WithEx[*QuotaData](time.Duration(DefaultWindowHours)*time.Hour))
	} else {
		quotaCache.Del(connID)
	}

	remaining := limit - len(valid)
	if remaining < 0 {
		remaining = 0
	}

	return remaining, nil
}

// Increment adds a download to the quota for a connection
// count parameter allows for multiple downloads (e.g., range requests)
func Increment(connID string, count int) error {
	if count <= 0 {
		count = 1
	}

	now := time.Now()
	data, exists := quotaCache.Get(connID)
	if !exists {
		data = &QuotaData{
			Timestamps: make([]time.Time, 0, count),
		}
	}

	// Add new timestamps
	for i := 0; i < count; i++ {
		data.Timestamps = append(data.Timestamps, now)
	}

	// Store with 24h TTL
	quotaCache.Set(connID, data, cache.WithEx[*QuotaData](time.Duration(DefaultWindowHours)*time.Hour))

	return nil
}

// GetWindowStart returns the start time of the sliding window
func GetWindowStart() time.Time {
	return time.Now().Add(-time.Duration(DefaultWindowHours) * time.Hour)
}
