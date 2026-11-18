package mcp

import (
	"github.com/stretchr/testify/require"
	"reflect"
	"testing"
	"unsafe"
)

func TestCheckedReadWitnessFitsExistingProjectionTailPadding(t *testing.T) {
	typ := reflect.TypeOf(projectionEntry{})
	retirement, ok := typ.FieldByName("checkedRetired")
	require.True(t, ok)
	witness, ok := typ.FieldByName("readVersionChecked")
	require.True(t, ok)
	align := uintptr(typ.Align())
	oldEnd := retirement.Offset + retirement.Type.Size()
	oldSize := (oldEnd + align - 1) &^ (align - 1)
	actual := unsafe.Sizeof(projectionEntry{})
	t.Logf("projection_entry_size=%d pre_witness_size=%d alignment=%d retirement_offset=%d retirement_bytes=%d witness_offset=%d witness_bytes=%d tail_padding_before=%d", actual, oldSize, align, retirement.Offset, retirement.Type.Size(), witness.Offset, witness.Type.Size(), oldSize-oldEnd)
	require.Equal(t, typ.Size(), actual)
	require.Equal(t, oldSize, actual, "witness must fit existing tail padding, preserving the accepted512 memory premise")
	require.GreaterOrEqual(t, witness.Offset, oldEnd)
	require.LessOrEqual(t, witness.Offset+witness.Type.Size(), oldSize)
}
