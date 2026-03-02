package quota

import (
	"sync"
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

// quotaMutex ensures atomic check-and-reserve operations
var quotaMutex sync.Mutex

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

// CheckAndReserve atomically checks if quota is available and reserves it.
// This prevents race conditions when multiple concurrent downloads occur.
// Returns: allowed (bool), remaining (int), error
func CheckAndReserve(connID string, count int, limit int) (bool, int, error) {
	quotaMutex.Lock()
	defer quotaMutex.Unlock()

	if limit <= 0 {
		limit = DefaultLimit
	}

	cutoff := time.Now().Add(-time.Duration(DefaultWindowHours) * time.Hour)

	// Get current data
	data, exists := quotaCache.Get(connID)
	if !exists {
		data = &QuotaData{Timestamps: make([]time.Time, 0, count)}
	}

	// Filter valid timestamps within the sliding window
	valid := make([]time.Time, 0, len(data.Timestamps))
	for _, ts := range data.Timestamps {
		if ts.After(cutoff) {
			valid = append(valid, ts)
		}
	}

	remaining := limit - len(valid)
	if remaining < 0 {
		remaining = 0
	}

	// Check if enough quota available
	if remaining < count {
		return false, remaining, nil
	}

	// Reserve quota by adding timestamps immediately
	now := time.Now()
	for i := 0; i < count; i++ {
		valid = append(valid, now)
	}

	// Update cache with reserved quota
	quotaCache.Set(connID, &QuotaData{Timestamps: valid},
		cache.WithEx[*QuotaData](time.Duration(DefaultWindowHours)*time.Hour))

	return true, remaining - count, nil
}

// Decrement removes count from quota (used for rollback when downloads fail)
func Decrement(connID string, count int) error {
	quotaMutex.Lock()
	defer quotaMutex.Unlock()

	if count <= 0 {
		return nil
	}

	data, exists := quotaCache.Get(connID)
	if !exists || len(data.Timestamps) == 0 {
		return nil
	}

	// Remove the most recent 'count' timestamps
	if count >= len(data.Timestamps) {
		quotaCache.Del(connID)
	} else {
		data.Timestamps = data.Timestamps[:len(data.Timestamps)-count]
		quotaCache.Set(connID, data,
			cache.WithEx[*QuotaData](time.Duration(DefaultWindowHours)*time.Hour))
	}

	return nil
}
