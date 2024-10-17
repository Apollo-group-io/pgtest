package pgtest_test

import (
	"testing"

	"github.com/Apollo-group-io/pgtest"
	"github.com/stretchr/testify/assert"
)

func TestPGConfig(t *testing.T) {
	assert := assert.New(t)

	config := pgtest.New().From("/usr/bin").DataDir("/tmp/data").Persistent().EnableFSync().WithAdditionalArgs("-c", "log_statement=all")

	assert.True(config.IsPersistent)
	assert.EqualValues("/tmp/data", config.Dir)
	assert.EqualValues("/usr/bin", config.BinDir)
	assert.EqualValues([]string{"-c", "log_statement=all"}, config.AdditionalArgs)
	assert.EqualValues(true, config.FSync)
}
