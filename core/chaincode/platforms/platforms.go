/*
# Copyright IBM Corp. All Rights Reserved.
#
# SPDX-License-Identifier: Apache-2.0
*/

package platforms

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"

	"strings"

	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/common/metadata"
	cutil "github.com/hyperledger/fabric/core/container/util"
	pb "github.com/hyperledger/fabric/protos/peer"
)

//MetadataProvider is implemented by each platform in a platform specific manner.
//It can process metadata stored in ChaincodeDeploymentSpec in different formats.
//The common format is targz. Currently users expect the metadata to be presented
//as tar file entries (directly extracted from chaincode stored in targz format).
//In future, we would like provide better abstraction by extending the interface
type MetadataProvider interface {
	GetMetadataAsTarEntries() ([]byte, error)
}

// Interface for validating the specification and and writing the package for
// the given platform
type Platform interface {
	Name() string
	ValidatePath(path string) error
	ValidateCodePackage(code []byte) error
	GetDeploymentPayload(path string) ([]byte, error)
	GenerateDockerfile() (string, error)
	GenerateDockerBuild(path string, code []byte, tw *tar.Writer) error
	GetMetadataProvider(spec *pb.ChaincodeDeploymentSpec) MetadataProvider
}

type PackageWriter interface {
	Write(name string, payload []byte, tw *tar.Writer) error
}

type PackageWriterWrapper func(name string, payload []byte, tw *tar.Writer) error

func (pw PackageWriterWrapper) Write(name string, payload []byte, tw *tar.Writer) error {
	return pw(name, payload, tw)
}

type Registry struct {
	Platforms     map[string]Platform
	PackageWriter PackageWriter
}

var logger = flogging.MustGetLogger("chaincode-platform")

func NewRegistry(platformTypes ...Platform) *Registry {
	platforms := make(map[string]Platform)
	for _, platform := range platformTypes {
		if _, ok := platforms[platform.Name()]; ok {
			logger.Panicf("Multiple platforms of the same name specified: %s", platform.Name())
		}
		platforms[platform.Name()] = platform
	}
	return &Registry{
		Platforms:     platforms,
		PackageWriter: PackageWriterWrapper(cutil.WriteBytesToPackage),
	}
}

func (r *Registry) ValidateSpec(spec *pb.ChaincodeSpec) error {
	platform, ok := r.Platforms[spec.Type.String()]
	if !ok {
		return fmt.Errorf("Unknown chaincodeType: %s", spec.Type)
	}
	return platform.ValidatePath(spec.Path())
}

func (r *Registry) ValidateDeploymentSpec(spec *pb.ChaincodeDeploymentSpec) error {
	platform, ok := r.Platforms[spec.ChaincodeSpec.Type.String()]
	if !ok {
		return fmt.Errorf("Unknown chaincodeType: %s", spec.ChaincodeSpec.Type)
	}
	return platform.ValidateCodePackage(spec.Bytes())
}

func (r *Registry) GetMetadataProvider(spec *pb.ChaincodeDeploymentSpec) (MetadataProvider, error) {
	platform, ok := r.Platforms[spec.ChaincodeSpec.Type.String()]
	if !ok {
		return nil, fmt.Errorf("Unknown chaincodeType: %s", spec.ChaincodeSpec.Type)
	}
	return platform.GetMetadataProvider(spec), nil
}

func (r *Registry) GetDeploymentPayload(spec *pb.ChaincodeSpec) ([]byte, error) {
	platform, ok := r.Platforms[spec.Type.String()]
	if !ok {
		return nil, fmt.Errorf("Unknown chaincodeType: %s", spec.Type)
	}

	return platform.GetDeploymentPayload(spec.Path())
}

func (r *Registry) GenerateDockerfile(cds *pb.ChaincodeDeploymentSpec) (string, error) {
	platform, ok := r.Platforms[cds.ChaincodeSpec.Type.String()]
	if !ok {
		return "", fmt.Errorf("Unknown chaincodeType: %s", cds.ChaincodeSpec.Type)
	}

	var buf []string

	// ----------------------------------------------------------------------------------------------------
	// Let the platform define the base Dockerfile
	// ----------------------------------------------------------------------------------------------------
	base, err := platform.GenerateDockerfile()
	if err != nil {
		return "", fmt.Errorf("Failed to generate platform-specific Dockerfile: %s", err)
	}
	buf = append(buf, base)

	// ----------------------------------------------------------------------------------------------------
	// Add some handy labels
	// ----------------------------------------------------------------------------------------------------
	buf = append(buf, fmt.Sprintf("LABEL %s.chaincode.id.name=\"%s\" \\", metadata.BaseDockerLabel, cds.ChaincodeSpec.ChaincodeId.Name))
	buf = append(buf, fmt.Sprintf("      %s.chaincode.id.version=\"%s\" \\", metadata.BaseDockerLabel, cds.ChaincodeSpec.ChaincodeId.Version))
	buf = append(buf, fmt.Sprintf("      %s.chaincode.type=\"%s\" \\", metadata.BaseDockerLabel, cds.ChaincodeSpec.Type.String()))
	buf = append(buf, fmt.Sprintf("      %s.version=\"%s\" \\", metadata.BaseDockerLabel, metadata.Version))
	buf = append(buf, fmt.Sprintf("      %s.base.version=\"%s\"", metadata.BaseDockerLabel, metadata.BaseVersion))
	// ----------------------------------------------------------------------------------------------------
	// Then augment it with any general options
	// ----------------------------------------------------------------------------------------------------
	//append version so chaincode build version can be compared against peer build version
	buf = append(buf, fmt.Sprintf("ENV CORE_CHAINCODE_BUILDLEVEL=%s", metadata.Version))

	// ----------------------------------------------------------------------------------------------------
	// Finalize it
	// ----------------------------------------------------------------------------------------------------
	contents := strings.Join(buf, "\n")
	logger.Debugf("\n%s", contents)

	return contents, nil
}

func (r *Registry) StreamDockerBuild(cds *pb.ChaincodeDeploymentSpec, inputFiles map[string][]byte, tw *tar.Writer) error {
	var err error

	// ----------------------------------------------------------------------------------------------------
	// Determine our platform driver from the spec
	// ----------------------------------------------------------------------------------------------------
	platform, ok := r.Platforms[cds.ChaincodeSpec.Type.String()]
	if !ok {
		return fmt.Errorf("could not find platform of type: %s", cds.ChaincodeSpec.Type.String())
	}

	// ----------------------------------------------------------------------------------------------------
	// First stream out our static inputFiles
	// ----------------------------------------------------------------------------------------------------
	for name, data := range inputFiles {
		err = r.PackageWriter.Write(name, data, tw)
		if err != nil {
			return fmt.Errorf("Failed to inject \"%s\": %s", name, err)
		}
	}

	// ----------------------------------------------------------------------------------------------------
	// Now give the platform an opportunity to contribute its own context to the build
	// ----------------------------------------------------------------------------------------------------
	err = platform.GenerateDockerBuild(cds.Path(), cds.Bytes(), tw)
	if err != nil {
		return fmt.Errorf("Failed to generate platform-specific docker build: %s", err)
	}

	return nil
}

func (r *Registry) GenerateDockerBuild(cds *pb.ChaincodeDeploymentSpec) (io.Reader, error) {

	inputFiles := make(map[string][]byte)

	// ----------------------------------------------------------------------------------------------------
	// Generate the Dockerfile specific to our context
	// ----------------------------------------------------------------------------------------------------
	dockerFile, err := r.GenerateDockerfile(cds)
	if err != nil {
		return nil, fmt.Errorf("Failed to generate a Dockerfile: %s", err)
	}

	inputFiles["Dockerfile"] = []byte(dockerFile)

	// ----------------------------------------------------------------------------------------------------
	// Finally, launch an asynchronous process to stream all of the above into a docker build context
	// ----------------------------------------------------------------------------------------------------
	input, output := io.Pipe()

	go func() {
		gw := gzip.NewWriter(output)
		tw := tar.NewWriter(gw)
		err := r.StreamDockerBuild(cds, inputFiles, tw)
		if err != nil {
			logger.Error(err)
		}

		tw.Close()
		gw.Close()
		output.CloseWithError(err)
	}()

	return input, nil
}
