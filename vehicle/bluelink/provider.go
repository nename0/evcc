package bluelink

import (
	"fmt"
	"sync"
	"time"

	"github.com/evcc-io/evcc/api"
	// "github.com/evcc-io/evcc/util"
)

// minimal interval to wait between wakeups
const wakeupTimeout = 5 * time.Minute

// Provider implements the vehicle api.
// Based on https://github.com/Hacksore/bluelinky.
type Provider struct {
	api     *API
	vehicle Vehicle

	statusAge   time.Duration
	cacheExpiry time.Duration

	mu              sync.Mutex
	fetchStatusTime time.Time
	forceUpdateTime time.Time

	cachedStatusValid   bool
	fetchStatusHadError bool

	cachedStatus             VehicleStatus
	cachedVehicleLocationLat float64
	cachedVehicleLocationLon float64
	cachedOdometer           float64
}

// NewProvider creates a new BlueLink API
func NewProvider(api *API, vehicle Vehicle, statusAge, cacheExpiry time.Duration) *Provider {
	v := &Provider{
		api:     api,
		vehicle: vehicle,

		statusAge:   statusAge,
		cacheExpiry: cacheExpiry,
	}

	return v
}

func (v *Provider) fetchServerStatus() error {
	v.fetchStatusTime = time.Now()
	serverStatus, err := v.api.StatusLatest(v.vehicle)
	if err != nil {
		fmt.Println("Bluelink: fetchServerStatus(), err:", err)
		v.fetchStatusHadError = true
	} else {
		fmt.Println("Bluelink: fetchServerStatus(): success")
		v.fetchStatusHadError = false
		v.cachedStatusValid = true
		v.cachedStatus = serverStatus.ResMsg.VehicleStatusInfo.VehicleStatus
		v.cachedVehicleLocationLat = serverStatus.ResMsg.VehicleStatusInfo.VehicleLocation.Coord.Lat
		v.cachedVehicleLocationLon = serverStatus.ResMsg.VehicleStatusInfo.VehicleLocation.Coord.Lon
		v.cachedOdometer = serverStatus.ResMsg.VehicleStatusInfo.Odometer.Value
	}
	return err
}

func (v *Provider) forceStatusUpdate() error {
	v.forceUpdateTime = time.Now()
	serverStatus, err := v.api.StatusPartial(v.vehicle)
	if err != nil {
		// it always returns an error, but it actually works. So fetch the updated state from the server
		fmt.Println("Bluelink: expected forceStatusUpdate() err -> fetchServerStatus()")
		return v.fetchServerStatus()
	} else {
		fmt.Println("Bluelink: forceStatusUpdate(): no error !! ")
		v.cachedStatusValid = true
		v.cachedStatus = serverStatus.ResMsg
	}
	return err
}

// status wraps the two api status calls and adds status refresh
func (v *Provider) status() (VehicleStatus, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	// don't fetch the status if we did so in the last v.cacheExpiry
	shouldFetchStatus := time.Since(v.fetchStatusTime) > v.cacheExpiry && time.Since(v.forceUpdateTime) > v.cacheExpiry
	for {
		// skip for first time with invalid status
		if v.cachedStatusValid {
			updated, err := v.cachedStatus.Updated()
			if err != nil {
				return VehicleStatus{}, err
			}
			if time.Since(updated) <= v.statusAge {
				// cachedStatus is 'recent'
				return v.cachedStatus, nil
			}
		}
		if !shouldFetchStatus {
			// fetched status is old -> force status update
			break
		}
		// check if status on server updated before forcing update
		err := v.fetchServerStatus()
		if err != nil {
			return VehicleStatus{}, err
		}
		shouldFetchStatus = false
	}

	if time.Since(v.forceUpdateTime) <= v.cacheExpiry {
		// return the cached version if we forced an update in the last v.cacheExpiry
		if v.fetchStatusHadError {
			return VehicleStatus{}, api.ErrMustRetry
		}
		return v.cachedStatus, nil
	}

	err := v.forceStatusUpdate()
	if err != nil {
		return VehicleStatus{}, err
	}
	return v.cachedStatus, nil
}

// forceStatusUpdate() does not include location or odometer in the response, so it needs its own getter
func (v *Provider) locationAndOdometer() (lat float64, lon float64, odometer float64, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	// check if max(v.statusAge, v.cacheExpiry) has elapsed since the last fetchStatus
	timeSinceFetch := time.Since(v.fetchStatusTime)
	if timeSinceFetch <= v.statusAge ||
		timeSinceFetch <= v.cacheExpiry {
		if v.fetchStatusHadError {
			return v.cachedVehicleLocationLat, v.cachedVehicleLocationLon, v.cachedOdometer, api.ErrMustRetry
		}
		return v.cachedVehicleLocationLat, v.cachedVehicleLocationLon, v.cachedOdometer, nil
	}
	// we do not use v.cachedStatus.Updated() here,
	// as the location should use v.cachedVehicleLocation.Time
	// which might be older

	// TODO bluelink: improve api.VehiclePosition:
	// - force a location update using the 'vehicles/%s/location' endpoint
	// - parse v.cachedVehicleLocation.Time to check expiry

	// just re-fetch the status maybe it updated maybe not
	err = v.fetchServerStatus()
	return v.cachedVehicleLocationLat, v.cachedVehicleLocationLon, v.cachedOdometer, err
}

var _ api.Battery = (*Provider)(nil)

// Soc implements the api.Battery interface
func (v *Provider) Soc() (float64, error) {
	res, err := v.status()
	if err != nil {
		return 0, err
	}
	return res.SoC()
}

var _ api.ChargeState = (*Provider)(nil)

// Status implements the api.ChargeState interface
func (v *Provider) Status() (api.ChargeStatus, error) {
	status := api.StatusNone
	res, err := v.status()
	if err != nil {
		return status, err
	}
	return res.Status()
}

var _ api.VehicleFinishTimer = (*Provider)(nil)

// FinishTime implements the api.VehicleFinishTimer interface
func (v *Provider) FinishTime() (time.Time, error) {
	res, err := v.status()
	if err != nil {
		return time.Time{}, err
	}
	return res.FinishTime()
}

var _ api.VehicleRange = (*Provider)(nil)

// Range implements the api.VehicleRange interface
func (v *Provider) Range() (int64, error) {
	res, err := v.status()
	if err != nil {
		return 0, err
	}
	return res.Range()
}

var _ api.VehicleOdometer = (*Provider)(nil)

// Odometer implements the api.VehicleOdometer interface
func (v *Provider) Odometer() (float64, error) {
	_, _, odometer, err := v.locationAndOdometer()
	return odometer, err
}

var _ api.VehicleClimater = (*Provider)(nil)

// Climater implements the api.VehicleClimater interface
func (v *Provider) Climater() (bool, error) {
	res, err := v.status()
	if err != nil {
		return false, err
	}
	return res.Climater()
}

var _ api.SocLimiter = (*Provider)(nil)

// GetLimitSoc implements the api.SocLimiter interface
func (v *Provider) GetLimitSoc() (int64, error) {
	res, err := v.status()
	if err != nil {
		return 0, err
	}
	return res.GetLimitSoc()
}

var _ api.VehiclePosition = (*Provider)(nil)

// Position implements the api.VehiclePosition interface
func (v *Provider) Position() (float64, float64, error) {
	lat, lon, _, err := v.locationAndOdometer()
	return lat, lon, err
}

var _ api.Resurrector = (*Provider)(nil)

// WakeUp implements the api.Resurrector interface
func (v *Provider) WakeUp() error {
	fmt.Println("waking up vehicle")
	v.mu.Lock()
	defer v.mu.Unlock()

	if time.Since(v.forceUpdateTime) > wakeupTimeout {
		// forcing an update will usually make the car start charging even if the (first) resulting status still says it does not charge...
		return v.forceStatusUpdate()
	}
	// do nothing if we already forced an update in the last wakeupTimeout
	return nil
}
