/*
Licensed to the Apache Software Foundation (ASF) under one or more
contributor license agreements.  See the NOTICE file distributed with
this work for additional information regarding copyright ownership.
The ASF licenses this file to You under the Apache License, Version 2.0
(the "License"); you may not use this file except in compliance with
the License.  You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package virtualdatabase

import (
	"context"
	"fmt"
	"os"
	"strings"

	obuildv1 "github.com/openshift/api/build/v1"
	scheme "github.com/openshift/client-go/build/clientset/versioned/scheme"
	"github.com/teiid/teiid-operator/pkg/apis/teiid/v1alpha1"
	"github.com/teiid/teiid-operator/pkg/controller/virtualdatabase/constants"
	"github.com/teiid/teiid-operator/pkg/util"
	"github.com/teiid/teiid-operator/pkg/util/envvar"
	"github.com/teiid/teiid-operator/pkg/util/image"
	"github.com/teiid/teiid-operator/pkg/util/maven"
	"github.com/teiid/teiid-operator/pkg/util/proxy"
	"github.com/teiid/teiid-operator/pkg/util/vdbutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// News2IBuilderImageAction creates a new initialize action
func News2IBuilderImageAction() Action {
	return &s2iBuilderImageAction{}
}

type s2iBuilderImageAction struct {
	baseAction
}

// Name returns a common name of the action
func (action *s2iBuilderImageAction) Name() string {
	return "S2IBuilderImageAction"
}

// CanHandle tells whether this action can handle the virtualdatabase
func (action *s2iBuilderImageAction) CanHandle(vdb *v1alpha1.VirtualDatabase) bool {
	return vdb.Status.Phase == v1alpha1.ReconcilerPhaseS2IReady || vdb.Status.Phase == v1alpha1.ReconcilerPhaseBuilderImage
}

// Handle handles the virtualdatabase
func (action *s2iBuilderImageAction) Handle(ctx context.Context, vdb *v1alpha1.VirtualDatabase, r *ReconcileVirtualDatabase) error {

	if vdb.Status.Phase == v1alpha1.ReconcilerPhaseS2IReady {
		vdb.Status.Phase = v1alpha1.ReconcilerPhaseBuilderImage

		opDeployment := &appsv1.Deployment{}
		opDeploymentNS := os.Getenv("WATCH_NAMESPACE")
		opDeploymentName := os.Getenv("OPERATOR_NAME")
		r.client.Get(ctx, types.NamespacedName{Namespace: opDeploymentNS, Name: opDeploymentName}, opDeployment)

		log.Info("Building Base builder Image")
		// Define new BuildConfig objects
		buildConfig, err := action.buildBC(vdb, r)
		if err != nil {
			return err
		}
		// set ownerreference for service BC only
		if _, err := image.EnsureImageStream(buildConfig.Name, vdb.ObjectMeta.Namespace, true, opDeployment, r.imageClient, r.client.GetScheme()); err != nil {
			return err
		}

		// check to make sure the base s2i image for the build is available
		isName := buildConfig.Spec.Strategy.SourceStrategy.From.Name
		isNameSpace := buildConfig.Spec.Strategy.SourceStrategy.From.Namespace
		_, err = r.imageClient.ImageStreamTags(isNameSpace).Get(isName, metav1.GetOptions{})
		if err != nil && errors.IsNotFound(err) {
			log.Warn(isNameSpace, "/", isName, " ImageStreamTag does not exist and is required for this build.")
			return err
		} else if err != nil {
			return err
		}

		// Check if this BC already exists
		bc, err := r.buildClient.BuildConfigs(buildConfig.Namespace).Get(buildConfig.Name, metav1.GetOptions{})
		if err != nil && errors.IsNotFound(err) {
			log.Info("Creating a new BuildConfig ", buildConfig.Name, " in namespace ", buildConfig.Namespace)

			// make the Operator as the owner
			err := controllerutil.SetControllerReference(opDeployment, &buildConfig, r.client.GetScheme())
			if err != nil {
				log.Error(err)
			}

			bc, err = r.buildClient.BuildConfigs(buildConfig.Namespace).Create(&buildConfig)
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		}

		log.Info("Created BuildConfig")

		// Trigger first build of "builder" and binary BCs
		if bc.Status.LastVersion == 0 {
			log.Info("triggering the base builder image build")
			mavenRepos := constants.GetMavenRepositories(vdb)
			if err = action.triggerBuild(ctx, *bc, mavenRepos, r); err != nil {
				return err
			}
		} else {
			// if in case nay previous build failed try again
			builds, err := getBuilds(vdb, r)
			if err != nil {
				return err
			}
			for _, build := range builds.Items {
				if build.Status.Phase == obuildv1.BuildPhaseError || build.Status.Phase == obuildv1.BuildPhaseFailed || build.Status.Phase == obuildv1.BuildPhaseCancelled {
					log.Info("triggering the base builder image build")
					mavenRepos := constants.GetMavenRepositories(vdb)
					if err = action.triggerBuild(ctx, *bc, mavenRepos, r); err != nil {
						return err
					}
				}
			}
		}
	} else if vdb.Status.Phase == v1alpha1.ReconcilerPhaseBuilderImage {
		builds, err := getBuilds(vdb, r)
		if err != nil {
			return err
		}
		for _, build := range builds.Items {
			// set status of the build
			if build.Status.Phase == obuildv1.BuildPhaseComplete && vdb.Status.Phase != v1alpha1.ReconcilerPhaseBuilderImageFinished {
				vdb.Status.Phase = v1alpha1.ReconcilerPhaseBuilderImageFinished
			} else if (build.Status.Phase == obuildv1.BuildPhaseError ||
				build.Status.Phase == obuildv1.BuildPhaseFailed ||
				build.Status.Phase == obuildv1.BuildPhaseCancelled) && vdb.Status.Phase != v1alpha1.ReconcilerPhaseBuilderImageFailed {
				vdb.Status.Phase = v1alpha1.ReconcilerPhaseBuilderImageFailed
			} else if build.Status.Phase == obuildv1.BuildPhaseRunning && vdb.Status.Phase != v1alpha1.ReconcilerPhaseBuilderImage {
				vdb.Status.Phase = v1alpha1.ReconcilerPhaseBuilderImage
			}
		}
	}
	return nil
}

func getBuilds(vdb *v1alpha1.VirtualDatabase, r *ReconcileVirtualDatabase) (*obuildv1.BuildList, error) {
	builds := &obuildv1.BuildList{}
	options := metav1.ListOptions{
		FieldSelector: "metadata.namespace=" + vdb.ObjectMeta.Namespace,
		LabelSelector: "buildconfig=" + constants.BuilderImageTargetName,
	}
	builds, err := r.buildClient.Builds(vdb.ObjectMeta.Namespace).List(options)
	if err != nil {
		return builds, err
	}
	return builds, nil
}

// newBCForCR returns a BuildConfig with the same name/namespace as the cr
func (action *s2iBuilderImageAction) buildBC(vdb *v1alpha1.VirtualDatabase, r *ReconcileVirtualDatabase) (obuildv1.BuildConfig, error) {
	bc := obuildv1.BuildConfig{}
	envs := []corev1.EnvVar{}

	// handle proxy settings
	envs, jp := proxy.HTTPSettings(envs)
	var javaProperties string
	for k, v := range jp {
		javaProperties = javaProperties + "-D" + k + "=" + v + " "
	}

	str := strings.Join([]string{
		" ",
		"-Djava.net.useSystemProxies=true",
		"-Dmaven.compiler.source=1.8",
		"-Dmaven.compiler.target=1.8",
	}, " ")

	envvar.SetVal(&envs, "DEPLOYMENTS_DIR", "/tmp") // this is avoid copying the jar file
	envvar.SetVal(&envs, "MAVEN_ARGS_APPEND", "clean package "+javaProperties+str)
	envvar.SetVal(&envs, "ARTIFACT_DIR", "target/")

	incremental := true
	bi := constants.Config.BuildImage
	imageName := fmt.Sprintf("%s:%s", bi.ImageName, bi.Tag)
	//isNamespace := vdb.ObjectMeta.Namespace
	// check if the base image is found otherwise use from dockerhub, add to local images
	if !image.CheckImageStream(bi.ImageName, vdb.ObjectMeta.Namespace, r.imageClient) {
		dockerImage := fmt.Sprintf("%s/%s/%s", bi.Registry, bi.ImagePrefix, bi.ImageName)
		err := image.CreateImageStream(bi.ImageName, vdb.ObjectMeta.Namespace, dockerImage, bi.Tag, r.imageClient, r.client.GetScheme())
		if err != nil {
			return bc, err
		}
	}

	builderName := constants.BuilderImageTargetName
	bc = obuildv1.BuildConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      builderName,
			Namespace: vdb.ObjectMeta.Namespace,
		},
	}
	bc.SetGroupVersionKind(obuildv1.SchemeGroupVersion.WithKind("BuildConfig"))
	bc.Spec.Source.Binary = &obuildv1.BinaryBuildSource{}
	bc.Spec.Output.To = &corev1.ObjectReference{Name: strings.Join([]string{builderName, "latest"}, ":"), Kind: "ImageStreamTag"}
	bc.Spec.Strategy.Type = obuildv1.SourceBuildStrategyType
	bc.Spec.Strategy.SourceStrategy = &obuildv1.SourceBuildStrategy{
		Incremental: &incremental,
		Env:         envs,
		From: corev1.ObjectReference{
			Name:      imageName,
			Namespace: vdb.ObjectMeta.Namespace,
			Kind:      "ImageStreamTag",
		},
	}
	return bc, nil
}

// triggerBuild triggers a BuildConfig to start a new build
func (action *s2iBuilderImageAction) triggerBuild(ctx context.Context, bc obuildv1.BuildConfig, mavenRepositories map[string]string, r *ReconcileVirtualDatabase) error {
	log := log.With("kind", "BuildConfig", "name", bc.GetName(), "namespace", bc.GetNamespace())
	log.Info("starting the build for base image")
	buildConfig, err := r.buildClient.BuildConfigs(bc.Namespace).Get(bc.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	vdbCopy := &v1alpha1.VirtualDatabase{}
	vdbCopy.ObjectMeta.Name = "virtualdatabase-image"
	vdbCopy.ObjectMeta.Namespace = bc.GetNamespace()
	vdbCopy.Spec.Build.Source.DDL = action.ddlFile()
	vdbCopy.Spec.Build.Source.MavenRepositories = mavenRepositories

	files := map[string]string{}

	pom, err := GenerateVdbPom(vdbCopy, vdbutil.ParseDataSourcesInfoFromDdl(vdbCopy.Spec.Build.Source.DDL), true, true, true)
	if err != nil {
		return err
	}

	// the below is to get copy plugin as dependency
	jarDependency, err := maven.ParseGAV("org.teiid:teiid-common-core:12.3.1")
	if err != nil {
		log.Error("The Maven based JAR is provided in bad format", err)
		return err
	}
	addCopyPlugIn(jarDependency, "jar", "app.jar", "/tmp", &pom)

	addVdbCodeGenPlugIn(&pom, "/tmp/src/src/main/resources/teiid.ddl", false, "0")
	pomContent, err := maven.EncodeXML(pom)
	if err != nil {
		return err
	}
	log.Debug(" Base Build Pom ", pomContent)

	// build default maven repository
	repositories := []maven.Repository{}
	mavenRepos := constants.GetMavenRepositories(vdbCopy)
	for k, v := range mavenRepos {
		repositories = append(repositories, maven.NewRepository(v+"@id="+k))
	}

	// read the settings file
	settingsContent, err := readMavenSettingsFile(ctx, vdbCopy, r, repositories)
	if err != nil {
		log.Debugf("Failed reading the settings.xml file for vdb %s", vdbCopy.ObjectMeta.Name)
		return err
	}

	log.Debugf("settings.xml file generated %s", settingsContent)

	files["/configuration/settings.xml"] = settingsContent
	files["/pom.xml"] = pomContent
	files["/src/main/resources/teiid.ddl"] = action.ddlFile()

	tarReader, err := util.Tar(files)
	if err != nil {
		return err
	}

	// do the binary build
	binaryBuildRequest := obuildv1.BinaryBuildRequestOptions{ObjectMeta: metav1.ObjectMeta{Name: buildConfig.Name}}
	binaryBuildRequest.SetGroupVersionKind(obuildv1.SchemeGroupVersion.WithKind("BinaryBuildRequestOptions"))
	log.Info("Triggering binary build ", buildConfig.Name)
	err = r.buildClient.RESTClient().Post().
		Namespace(bc.GetNamespace()).
		Resource("buildconfigs").
		Name(buildConfig.Name).
		SubResource("instantiatebinary").
		Body(tarReader).
		VersionedParams(&binaryBuildRequest, scheme.ParameterCodec).
		Do().
		Into(&obuildv1.Build{})
	if err != nil {
		return err
	}
	return nil
}

func (action *s2iBuilderImageAction) ddlFile() string {
	return `CREATE DATABASE customer OPTIONS (ANNOTATION 'Customer VDB');	
	USE DATABASE customer;
	CREATE FOREIGN DATA WRAPPER h2;
	CREATE SERVER mydb FOREIGN DATA WRAPPER h2;`
}
