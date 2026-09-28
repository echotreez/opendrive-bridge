package datacache

import (
	"reflect"
	"testing"
)

func TestChildrenOfSplitsFilesFromSubfolders(t *testing.T) {
	objs := []*Object{
		{RemotePath: "/b/a.bin"},
		{RemotePath: "/b/data/00/x"},
		{RemotePath: "/b/data/01/y"},
		{RemotePath: "/b/keys/k"},
		{RemotePath: "/other/z"},
		{RemotePath: "/bb/not-a-child"},
	}
	files, folders := ChildrenOf(objs, "/b")
	if len(files) != 1 || files[0].RemotePath != "/b/a.bin" {
		t.Errorf("files = %v", files)
	}
	if !reflect.DeepEqual(folders, []string{"data", "keys"}) {
		t.Errorf("folders = %v, want [data keys]", folders)
	}
	_, top := ChildrenOf(objs, "/")
	if !reflect.DeepEqual(top, []string{"b", "other", "bb"}) {
		t.Errorf("root folders = %v", top)
	}
}
