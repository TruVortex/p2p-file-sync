package diff

import (
	"p2p-file-sync/internal/proto"
	"path/filepath"
)

type DiffResult struct {
	Added    []*proto.ManifestNode
	Modified []*proto.ManifestNode
	Deleted  []string
}

func diffNode(
	local *proto.ManifestNode,
	remote *proto.ManifestNode,
	path string,
	result *DiffResult,
) {
	if local == nil && remote != nil {
		result.Added = append(result.Added, remote)
		return
	} else if local != nil && remote == nil {
		result.Deleted = append(result.Deleted, path)
		return
	} else if string(local.Hash) == string(remote.Hash) {
		return
	} else if !local.IsDir && !remote.IsDir {
		result.Modified = append(result.Modified, remote)
		return
	}
	localChildren := make(map[string]*proto.ManifestNode)
	remoteChildren := make(map[string]*proto.ManifestNode)
	for _, child := range local.Children {
		localChildren[child.Name] = child
	}
	for _, child := range remote.Children {
		remoteChildren[child.Name] = child
	}
	allNames := make(map[string]struct{})
	for name := range localChildren {
		allNames[name] = struct{}{}
	}
	for name := range remoteChildren {
		allNames[name] = struct{}{}
	}
	for name := range allNames {
		localChild := localChildren[name]
		remoteChild := remoteChildren[name]
		childPath := name
		if path != "" {
			childPath = filepath.Join(path, name)
		}
		diffNode(localChild, remoteChild, childPath, result)
	}
}

func DiffTrees(
	local *proto.ManifestNode,
	remote *proto.ManifestNode,
) *DiffResult {
	result := &DiffResult{}

	diffNode(local, remote, "", result)

	return result
}
