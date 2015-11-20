// Copyright 2015 Muir Manders.  All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package goftp

import (
	"os"
	"testing"
)

var goftpConfig = Config{
	User:     "anonymous",
	Password: "",
	//Logger: os.Stderr,
}

// list of addresses for tests to connect to
var ftpdAddrs []string = []string{"127.0.01:21"}
var implicitTLSAddrs = []string{"127.0.0.1:21"}

func TestMain(m *testing.M) {
	var ret int
	ret = m.Run()
	os.Exit(ret)
}
