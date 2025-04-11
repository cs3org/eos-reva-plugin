// Copyright 2018-2024 CERN
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// In applying this license, CERN does not waive the privileges and immunities
// granted to it by virtue of its status as an Intergovernmental Organization
// or submit itself to any jurisdiction.

package eosclient

import (
	"fmt"
	"strconv"

	"github.com/cs3org/reva/pkg/errtypes"
)

const (
	// SystemAttr is the system extended attribute.
	SystemAttr AttrType = iota
	// UserAttr is the user extended attribute.
	UserAttr
)

// AttrStringToType converts a string to an AttrType.
func AttrStringToType(t string) (AttrType, error) {
	switch t {
	case "sys":
		return SystemAttr, nil
	case "user":
		return UserAttr, nil
	default:
		return 0, errtypes.InternalError("attr type not existing")
	}
}

// AttrTypeToString converts a type to a string representation.
func AttrTypeToString(at AttrType) string {
	switch at {
	case SystemAttr:
		return "sys"
	case UserAttr:
		return "user"
	default:
		return "invalid"
	}
}

// GetKey returns the key considering the type of attribute.
func (a *Attribute) GetKey() string {
	return fmt.Sprintf("%s.%s", AttrTypeToString(a.Type), a.Key)
}

func GetDaemonAuth() Authorization {
	return Authorization{Role: Role{UID: "2", GID: "2"}}
}

// This function is used when we don't want to pass any additional auth info.
// Because we later populate the secret key for gRPC, we will be automatically
// mapped to cbox.
// So, in other words, use this function if you want to use the cbox account.
func GetEmptyAuth() Authorization {
	return Authorization{}
}

// Returns the userAuth if this is a valid auth object,
// otherwise returns daemonAuth
func GetUserOrDaemonAuth(userAuth Authorization) Authorization {
	if userAuth.Role.UID == "" || userAuth.Role.GID == "" {
		return GetDaemonAuth()
	} else {
		return userAuth
	}
}

// Extract uid and gid from auth object
func ExtractUidGid(auth Authorization) (uid, gid uint64, err error) {
	// $ id nobody
	// uid=65534(nobody) gid=65534(nobody) groups=65534(nobody)
	nobody := uint64(65534)

	uid, err = strconv.ParseUint(auth.Role.UID, 10, 64)
	if err != nil {
		return nobody, nobody, err
	}
	gid, err = strconv.ParseUint(auth.Role.GID, 10, 64)
	if err != nil {
		return nobody, nobody, err
	}

	return uid, gid, nil
}
