// Copyright 2018-2020 CERN
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

package ocis

import (
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	"github.com/cs3org/reva/pkg/appctx"
	"github.com/cs3org/reva/pkg/errtypes"
	"github.com/cs3org/reva/pkg/logger"
	"github.com/cs3org/reva/pkg/storage"
	"github.com/cs3org/reva/pkg/storage/fs/registry"
	"github.com/cs3org/reva/pkg/storage/utils/templates"
	"github.com/cs3org/reva/pkg/user"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
	"github.com/pkg/xattr"
)

const (
	// TODO the below comment is currently copied from the owncloud driver, revisit
	// Currently,extended file attributes have four separated
	// namespaces (user, trusted, security and system) followed by a dot.
	// A non root user can only manipulate the user. namespace, which is what
	// we will use to store ownCloud specific metadata. To prevent name
	// collisions with other apps We are going to introduce a sub namespace
	// "user.ocis."

	ocisPrefix   string = "user.ocis."
	parentidAttr string = ocisPrefix + "parentid"
	ownerIDAttr  string = ocisPrefix + "owner.id"
	ownerIDPAttr string = ocisPrefix + "owner.idp"
	// the base name of the node
	// updated when the file is renamed or moved
	nameAttr string = ocisPrefix + "name"

	// SharePrefix is the prefix for sharing related extended attributes
	sharePrefix    string = ocisPrefix + "acl."
	metadataPrefix string = ocisPrefix + "md."
	// TODO implement favorites metadata flag
	//favPrefix   string = ocisPrefix + "fav."  // favorite flag, per user

	// a temporary etag for a folder that is removed when the mtime propagation happens
	tmpEtagAttr   string = ocisPrefix + "tmp.etag"
	referenceAttr string = ocisPrefix + "cs3.ref" // arbitrary metadata
	//checksumPrefix    string = ocisPrefix + "cs."   // TODO add checksum support
	trashOriginAttr string = ocisPrefix + "trash.origin" // trash origin

	// we use a single attribute to enable or disable propagation of both: synctime and treesize
	propagationAttr string = ocisPrefix + "propagation"

	// the tree modification time of the tree below this node,
	// propagated when synctime_accounting is true and
	// user.ocis.propagation=1 is set
	// stored as a readable time.RFC3339Nano
	treeMTimeAttr string = ocisPrefix + "tmtime"

	// the size of the tree below this node,
	// propagated when treesize_accounting is true and
	// user.ocis.propagation=1 is set
	//treesizeAttr string = ocisPrefix + "treesize"
)

func init() {
	registry.Register("ocis", New)
}

func parseConfig(m map[string]interface{}) (*Path, error) {
	pw := &Path{}
	if err := mapstructure.Decode(m, pw); err != nil {
		err = errors.Wrap(err, "error decoding conf")
		return nil, err
	}
	return pw, nil
}

func (pw *Path) init(m map[string]interface{}) {
	if pw.UserLayout == "" {
		pw.UserLayout = "{{.Id.OpaqueId}}"
	}
	// ensure user layout has no starting or trailing /
	pw.UserLayout = strings.Trim(pw.UserLayout, "/")

	if pw.ShareFolder == "" {
		pw.ShareFolder = "/Shares"
	}
	// ensure share folder always starts with slash
	pw.ShareFolder = filepath.Join("/", pw.ShareFolder)

	// c.DataDirectory should never end in / unless it is the root
	pw.Root = filepath.Clean(pw.Root)
}

// New returns an implementation to of the storage.FS interface that talk to
// a local filesystem.
func New(m map[string]interface{}) (storage.FS, error) {
	pw, err := parseConfig(m)
	if err != nil {
		return nil, err
	}
	pw.init(m)

	dataPaths := []string{
		filepath.Join(pw.Root, "nodes"),
		// notes contain symlinks from nodes/<u-u-i-d>/uploads/<uploadid> to ../../uploads/<uploadid>
		// better to keep uploads on a fast / volatile storage before a workflow finally moves them to the nodes dir
		filepath.Join(pw.Root, "uploads"),
		filepath.Join(pw.Root, "trash"),
	}
	for _, v := range dataPaths {
		if err := os.MkdirAll(v, 0700); err != nil {
			logger.New().Error().Err(err).
				Str("path", v).
				Msg("could not create data dir")
		}
	}

	// the root node has an empty name, or use `.` ?
	// the root node has no parent, or use `root` ?
	if err = createNode(&Node{pw: pw, ID: "root"}, nil); err != nil {
		return nil, err
	}

	tp, err := NewTree(pw)
	if err != nil {
		return nil, err
	}

	return &ocisfs{
		tp: tp,
		pw: pw,
	}, nil
}

type ocisfs struct {
	tp TreePersistence
	pw *Path
}

func (fs *ocisfs) Shutdown(ctx context.Context) error {
	return nil
}

func (fs *ocisfs) GetQuota(ctx context.Context) (int, int, error) {
	return 0, 0, nil
}

// CreateHome creates a new root node that has no parent id
func (fs *ocisfs) CreateHome(ctx context.Context) (err error) {
	if !fs.pw.EnableHome || fs.pw.UserLayout == "" {
		return errtypes.NotSupported("ocisfs: CreateHome() home supported disabled")
	}

	var n, h *Node
	if n, err = fs.pw.RootNode(ctx); err != nil {
		return
	}
	h, err = fs.pw.WalkPath(ctx, n, fs.pw.mustGetUserLayout(ctx), func(ctx context.Context, n *Node) error {
		if !n.Exists {
			if err := fs.tp.CreateDir(ctx, n); err != nil {
				return err
			}
		}
		return nil
	})

	if fs.pw.TreeTimeAccounting {
		homePath := filepath.Join(fs.pw.Root, "nodes", h.ID)
		// mark the home node as the end of propagation
		if err = xattr.Set(homePath, propagationAttr, []byte("1")); err != nil {
			appctx.GetLogger(ctx).Error().Err(err).Interface("node", h).Msg("could not mark home as propagation root")
			return
		}
	}
	return
}

// GetHome is called to look up the home path for a user
// It is NOT supposed to return the internal path but the external path
func (fs *ocisfs) GetHome(ctx context.Context) (string, error) {
	if !fs.pw.EnableHome || fs.pw.UserLayout == "" {
		return "", errtypes.NotSupported("ocisfs: GetHome() home supported disabled")
	}
	u := user.ContextMustGetUser(ctx)
	layout := templates.WithUser(u, fs.pw.UserLayout)
	return filepath.Join(fs.pw.Root, layout), nil // TODO use a namespace?
}

// Tree persistence

// GetPathByID returns the fn pointed by the file id, without the internal namespace
func (fs *ocisfs) GetPathByID(ctx context.Context, id *provider.ResourceId) (string, error) {
	return fs.tp.GetPathByID(ctx, id)
}

func (fs *ocisfs) CreateDir(ctx context.Context, fn string) (err error) {
	var node *Node
	if node, err = fs.pw.NodeFromPath(ctx, fn); err != nil {
		return
	}
	if node.Exists {
		return errtypes.AlreadyExists(fn)
	}
	err = fs.tp.CreateDir(ctx, node)

	if fs.pw.TreeTimeAccounting {
		nodePath := filepath.Join(fs.pw.Root, "nodes", node.ID)
		// mark the home node as the end of propagation
		if err = xattr.Set(nodePath, propagationAttr, []byte("1")); err != nil {
			appctx.GetLogger(ctx).Error().Err(err).Interface("node", node).Msg("could not mark node to propagate")
			return
		}
	}
	return
}

// CreateReference creates a reference as a node folder with the target stored in extended attributes
// There is no difference between the /Shares folder and normal nodes because the storage is not supposed to be accessible without the storage provider.
// In effect everything is a shadow namespace.
// To mimic the eos end owncloud driver we only allow references as children of the "/Shares" folder
// TODO when home support is enabled should the "/Shares" folder still be listed?
func (fs *ocisfs) CreateReference(ctx context.Context, p string, targetURI *url.URL) (err error) {

	p = strings.Trim(p, "/")
	parts := strings.Split(p, "/")

	if len(parts) != 2 {
		return errtypes.PermissionDenied("ocisfs: references must be a child of the share folder: share_folder=" + fs.pw.ShareFolder + " path=" + p)
	}

	if parts[0] != strings.Trim(fs.pw.ShareFolder, "/") {
		return errtypes.PermissionDenied("ocisfs: cannot create references outside the share folder: share_folder=" + fs.pw.ShareFolder + " path=" + p)
	}

	// create Shares folder if it does not exist
	var n *Node
	if n, err = fs.pw.NodeFromPath(ctx, fs.pw.ShareFolder); err != nil {
		return errtypes.InternalError(err.Error())
	} else if !n.Exists {
		if err = fs.tp.CreateDir(ctx, n); err != nil {
			return
		}
	}

	if n, err = n.Child(parts[1]); err != nil {
		return errtypes.InternalError(err.Error())
	}

	if n.Exists {
		// TODO append increasing number to mountpoint name
		return errtypes.AlreadyExists(p)
	}

	if err = fs.tp.CreateDir(ctx, n); err != nil {
		return
	}

	internal := filepath.Join(fs.pw.Root, "nodes", n.ID)
	if err = xattr.Set(internal, referenceAttr, []byte(targetURI.String())); err != nil {
		return errors.Wrapf(err, "ocisfs: error setting the target %s on the reference file %s", targetURI.String(), internal)
	}
	return nil
}

func (fs *ocisfs) Move(ctx context.Context, oldRef, newRef *provider.Reference) (err error) {
	var oldNode, newNode *Node
	if oldNode, err = fs.pw.NodeFromResource(ctx, oldRef); err != nil {
		return
	}
	if !oldNode.Exists {
		err = errtypes.NotFound(filepath.Join(oldNode.ParentID, oldNode.Name))
		return
	}

	if newNode, err = fs.pw.NodeFromResource(ctx, newRef); err != nil {
		return
	}
	return fs.tp.Move(ctx, oldNode, newNode)
}

func (fs *ocisfs) GetMD(ctx context.Context, ref *provider.Reference, mdKeys []string) (ri *provider.ResourceInfo, err error) {
	var node *Node
	if node, err = fs.pw.NodeFromResource(ctx, ref); err != nil {
		return
	}
	if !node.Exists {
		err = errtypes.NotFound(filepath.Join(node.ParentID, node.Name))
		return
	}
	return node.AsResourceInfo(ctx)
}

func (fs *ocisfs) ListFolder(ctx context.Context, ref *provider.Reference, mdKeys []string) (finfos []*provider.ResourceInfo, err error) {
	var node *Node
	if node, err = fs.pw.NodeFromResource(ctx, ref); err != nil {
		return
	}
	if !node.Exists {
		err = errtypes.NotFound(filepath.Join(node.ParentID, node.Name))
		return
	}
	var children []*Node
	children, err = fs.tp.ListFolder(ctx, node)
	if err != nil {
		return
	}

	for i := range children {
		if ri, err := children[i].AsResourceInfo(ctx); err == nil {
			finfos = append(finfos, ri)
		}
	}
	return
}

func (fs *ocisfs) Delete(ctx context.Context, ref *provider.Reference) (err error) {
	var node *Node
	if node, err = fs.pw.NodeFromResource(ctx, ref); err != nil {
		return
	}
	if !node.Exists {
		err = errtypes.NotFound(filepath.Join(node.ParentID, node.Name))
		return
	}
	return fs.tp.Delete(ctx, node)
}

// Data persistence

func (fs *ocisfs) ContentPath(node *Node) string {
	return filepath.Join(fs.pw.Root, "nodes", node.ID)
}

func (fs *ocisfs) Download(ctx context.Context, ref *provider.Reference) (io.ReadCloser, error) {
	node, err := fs.pw.NodeFromResource(ctx, ref)
	if err != nil {
		return nil, errors.Wrap(err, "ocisfs: error resolving ref")
	}

	if !node.Exists {
		err = errtypes.NotFound(filepath.Join(node.ParentID, node.Name))
		return nil, err
	}

	contentPath := fs.ContentPath(node)

	r, err := os.Open(contentPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errtypes.NotFound(contentPath)
		}
		return nil, errors.Wrap(err, "ocisfs: error reading "+contentPath)
	}
	return r, nil
}

// arbitrary metadata persistence in metadata.go

// Version persistence in revisions.go

// Trash persistence in recycle.go

// share persistence in grants.go

func (fs *ocisfs) copyMD(s string, t string) (err error) {
	var attrs []string
	if attrs, err = xattr.List(s); err != nil {
		return err
	}
	for i := range attrs {
		if strings.HasPrefix(attrs[i], ocisPrefix) {
			var d []byte
			if d, err = xattr.Get(s, attrs[i]); err != nil {
				return err
			}
			if err = xattr.Set(t, attrs[i], d); err != nil {
				return err
			}
		}
	}
	return nil
}
