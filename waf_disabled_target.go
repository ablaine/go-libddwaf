// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

// Build when the target OS or architecture are not supported
//go:build (!linux && !darwin) || (!amd64 && !arm64)

package waf

import (
	"fmt"
	"runtime"
)

var disabledReason = fmt.Sprintf("the target operating-system %s or architecture %s are not supported", runtime.GOOS, runtime.GOARCH)
