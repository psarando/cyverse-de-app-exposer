package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"path"
	"strings"

	jobtmpl "github.com/cyverse-de/job-templates"
	"gopkg.in/cyverse-de/model.v4"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	analysisContainerName = "analysis"

	porklockConfigVolumeName = "porklock-config"
	porklockConfigSecretName = "porklock-config"
	porklockConfigMountPath  = "/etc/porklock"

	inputFilesVolumeName    = "input-files"
	inputFilesContainerName = "input-files"

	excludesMountPath  = "/excludes"
	excludesFileName   = "excludes-file"
	excludesVolumeName = "excludes-file"

	inputPathListMountPath  = "/input-paths"
	inputPathListFileName   = "input-path-list"
	inputPathListVolumeName = "input-path-list"

	outputFilesPortName = "tcp-output"
	inputFilesPortName  = "tcp-input"
	outputFilesPort     = int32(60000)
	inputFilesPort      = int32(60001)
)

func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }

func labelsFromJob(job *model.Job) map[string]string {
	return map[string]string{
		"app":      job.InvocationID,
		"app-name": job.AppName,
		"app-id":   job.AppID,
		"username": job.Submitter,
		"user-id":  job.UserID,
	}
}

func inputCommand(job *model.Job) []string {
	prependArgs := []string{
		"nc",
		"-lk",
		"-p", "60001",
		"-e",
		"porklock", "-jar", "/usr/src/app/porklock-standalone.jar",
	}
	appendArgs := []string{
		"-z", "/etc/porklock/irods-config.properties",
	}
	args := job.InputSourceListArguments(path.Join(inputPathListMountPath, inputPathListFileName))
	return append(append(prependArgs, args...), appendArgs...)
}

func analysisCommand(step *model.Step) []string {
	output := []string{}
	if step.Component.Container.EntryPoint != "" {
		output = append(output, step.Component.Container.EntryPoint)
	}
	if len(step.Arguments()) != 0 {
		output = append(output, step.Arguments()...)
	}
	return output
}

func analysisPorts(step *model.Step) []apiv1.ContainerPort {
	ports := []apiv1.ContainerPort{}

	for i, p := range step.Component.Container.Ports {
		ports = append(ports, apiv1.ContainerPort{
			ContainerPort: int32(p.ContainerPort),
			Name:          fmt.Sprintf("tcp-a-%d", i),
			Protocol:      apiv1.ProtocolTCP,
		})
	}

	return ports
}

func inputFilesMountPath(job *model.Job) string {
	return job.Steps[0].Component.Container.WorkingDirectory()
}

func excludesConfigMapName(job *model.Job) string {
	return fmt.Sprintf("excludes-file-%s", job.InvocationID)
}

func excludesConfigMap(job *model.Job) apiv1.ConfigMap {
	labels := labelsFromJob(job)

	return apiv1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   excludesConfigMapName(job),
			Labels: labels,
		},
		Data: map[string]string{
			excludesFileName: jobtmpl.ExcludesFileContents(job).String(),
		},
	}
}

func inputPathListConfigMapName(job *model.Job) string {
	return fmt.Sprintf("input-path-list-%s", job.InvocationID)
}

func (e *ExposerApp) inputPathListConfigMap(job *model.Job) (*apiv1.ConfigMap, error) {
	labels := labelsFromJob(job)

	fileContents, err := jobtmpl.InputPathListContents(job, e.InputPathListIdentifier, e.TicketInputPathListIdentifier)
	if err != nil {
		return nil, err
	}

	return &apiv1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   inputPathListConfigMapName(job),
			Labels: labels,
		},
		Data: map[string]string{
			inputPathListFileName: fileContents.String(),
		},
	}, nil
}

func outputCommand(job *model.Job) []string {
	prependArgs := []string{
		"nc", "-lk", "-p", "60000", "-e", "porklock", "-jar", "/usr/src/app/porklock-standalone.jar",
	}
	appendArgs := []string{
		"-z", "/etc/porklock/irods-config.properties",
	}
	args := job.FinalOutputArguments(path.Join(excludesMountPath, excludesFileName))
	return append(append(prependArgs, args...), appendArgs...)
}

func deploymentVolumes(job *model.Job) []apiv1.Volume {
	output := []apiv1.Volume{}

	if len(job.FilterInputsWithoutTickets()) > 0 {
		output = append(output, apiv1.Volume{
			Name: inputPathListVolumeName,
			VolumeSource: apiv1.VolumeSource{
				ConfigMap: &apiv1.ConfigMapVolumeSource{
					LocalObjectReference: apiv1.LocalObjectReference{
						Name: inputPathListConfigMapName(job),
					},
				},
			},
		})
	}

	output = append(output,
		apiv1.Volume{
			Name: inputFilesVolumeName,
			VolumeSource: apiv1.VolumeSource{
				EmptyDir: &apiv1.EmptyDirVolumeSource{},
			},
		},
		apiv1.Volume{
			Name: porklockConfigVolumeName,
			VolumeSource: apiv1.VolumeSource{
				Secret: &apiv1.SecretVolumeSource{
					SecretName: porklockConfigSecretName,
				},
			},
		},
		apiv1.Volume{
			Name: excludesVolumeName,
			VolumeSource: apiv1.VolumeSource{
				ConfigMap: &apiv1.ConfigMapVolumeSource{
					LocalObjectReference: apiv1.LocalObjectReference{
						Name: excludesConfigMapName(job),
					},
				},
			},
		},
	)

	return output
}

func (e *ExposerApp) deploymentContainers(job *model.Job) []apiv1.Container {
	output := []apiv1.Container{}

	if len(job.FilterInputsWithoutTickets()) > 0 {
		output = append(output, apiv1.Container{
			Name:       inputFilesContainerName,
			Image:      fmt.Sprintf("%s:%s", e.PorklockImage, e.PorklockTag),
			Command:    inputCommand(job),
			WorkingDir: inputPathListMountPath,
			VolumeMounts: []apiv1.VolumeMount{
				{
					Name:      porklockConfigVolumeName,
					MountPath: porklockConfigMountPath,
				},
				{
					Name:      inputFilesVolumeName,
					MountPath: inputFilesMountPath(job),
				},
			},
			Ports: []apiv1.ContainerPort{
				{
					Name:          inputFilesPortName,
					ContainerPort: inputFilesPort,
					Protocol:      apiv1.Protocol("TCP"),
				},
			},
			SecurityContext: &apiv1.SecurityContext{
				RunAsUser: int64Ptr(int64(job.Steps[0].Component.Container.UID)),
				Capabilities: &apiv1.Capabilities{
					Drop: []apiv1.Capability{
						"SETPCAP",
						"AUDIT_WRITE",
						"KILL",
						"SETGID",
						"SETUID",
						"NET_BIND_SERVICE",
						"SYS_CHROOT",
						"SETFCAP",
						"FSETID",
						"NET_RAW",
						"MKNOD",
					},
				},
			},
		})
	}

	output = append(output, apiv1.Container{
		Name: analysisContainerName,
		Image: fmt.Sprintf(
			"%s:%s",
			job.Steps[0].Component.Container.Image.Name,
			job.Steps[0].Component.Container.Image.Tag,
		),
		Command: analysisCommand(&job.Steps[0]),
		VolumeMounts: []apiv1.VolumeMount{
			{
				Name:      inputFilesVolumeName,
				MountPath: inputFilesMountPath(job),
			},
		},
		Ports: analysisPorts(&job.Steps[0]),
		SecurityContext: &apiv1.SecurityContext{
			RunAsUser: int64Ptr(int64(job.Steps[0].Component.Container.UID)),
			Capabilities: &apiv1.Capabilities{
				Drop: []apiv1.Capability{
					"SETPCAP",
					"AUDIT_WRITE",
					"KILL",
					"SETGID",
					"SETUID",
					"SYS_CHROOT",
					"SETFCAP",
					"FSETID",
					"MKNOD",
				},
			},
		},
	})

	output = append(output, apiv1.Container{
		Name: "output-files",
		Image: fmt.Sprintf(
			"%s:%s",
			e.PorklockImage,
			e.PorklockTag,
		),
		Command: outputCommand(job),
		VolumeMounts: []apiv1.VolumeMount{
			{
				Name:      porklockConfigVolumeName,
				MountPath: porklockConfigMountPath,
			},
			{
				Name:      inputFilesVolumeName,
				MountPath: inputFilesMountPath(job),
			},
			{
				Name:      excludesVolumeName,
				MountPath: excludesMountPath,
			},
		},
		Ports: []apiv1.ContainerPort{
			apiv1.ContainerPort{
				Name:          outputFilesPortName,
				ContainerPort: outputFilesPort,
			},
		},
		SecurityContext: &apiv1.SecurityContext{
			RunAsUser: int64Ptr(int64(job.Steps[0].Component.Container.UID)),
			Capabilities: &apiv1.Capabilities{
				Drop: []apiv1.Capability{
					"SETPCAP",
					"AUDIT_WRITE",
					"KILL",
					"SETGID",
					"SETUID",
					"NET_BIND_SERVICE",
					"SYS_CHROOT",
					"SETFCAP",
					"FSETID",
					"NET_RAW",
					"MKNOD",
				},
			},
		},
	})

	return output
}

func (e *ExposerApp) getDeployment(job *model.Job) (*appsv1.Deployment, error) {
	labels := labelsFromJob(job)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   job.InvocationID,
			Labels: labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": job.InvocationID,
				},
			},
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: apiv1.PodSpec{
					RestartPolicy: apiv1.RestartPolicy("Always"),
					Volumes:       deploymentVolumes(job),
					Containers:    e.deploymentContainers(job),
				},
			},
		},
	}

	b, _ := json.Marshal(deployment)
	log.Info(string(b))

	return deployment, nil
}

func (e *ExposerApp) createService(job *model.Job, deployment *appsv1.Deployment) apiv1.Service {
	labels := labelsFromJob(job)

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:   job.InvocationID,
			Labels: labels,
		},
		Spec: apiv1.ServiceSpec{
			Selector: map[string]string{
				"app": job.InvocationID,
			},
			Ports: []apiv1.ServicePort{
				apiv1.ServicePort{
					Name:       outputFilesPortName,
					Protocol:   apiv1.ProtocolTCP,
					Port:       outputFilesPort,
					TargetPort: intstr.FromString(outputFilesPortName),
				},
				apiv1.ServicePort{
					Name:       inputFilesPortName,
					Protocol:   apiv1.ProtocolTCP,
					Port:       inputFilesPort,
					TargetPort: intstr.FromString(inputFilesPortName),
				},
			},
		},
	}

	var analysisContainer apiv1.Container
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == analysisContainerName {
			analysisContainer = container
		}
	}

	for _, port := range analysisContainer.Ports {
		svc.Spec.Ports = append(svc.Spec.Ports, apiv1.ServicePort{
			Name:       port.Name,
			Protocol:   port.Protocol,
			Port:       port.ContainerPort,
			TargetPort: intstr.FromString(port.Name),
		})
	}

	return svc
}

func (e *ExposerApp) UpsertExcludesConfigMap(job *model.Job) error {
	excludesCM := excludesConfigMap(job)

	cmclient := e.clientset.CoreV1().ConfigMaps(e.viceNamespace)

	_, err := cmclient.Get(excludesConfigMapName(job), metav1.GetOptions{})
	if err != nil {
		fmt.Println(err)
		_, err = cmclient.Create(&excludesCM)
		if err != nil {
			return err
		}
	} else {
		_, err = cmclient.Update(&excludesCM)
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *ExposerApp) UpsertInputPathListConfigMap(job *model.Job) error {
	inputCM, err := e.inputPathListConfigMap(job)
	if err != nil {
		return err
	}

	cmclient := e.clientset.CoreV1().ConfigMaps(e.viceNamespace)

	_, err = cmclient.Get(inputPathListConfigMapName(job), metav1.GetOptions{})
	if err != nil {
		_, err = cmclient.Create(inputCM)
		if err != nil {
			return err
		}
	} else {
		_, err = cmclient.Update(inputCM)
		if err != nil {
			return err
		}
	}

	return nil
}

func (e *ExposerApp) UpsertDeployment(job *model.Job) error {
	deployment, err := e.getDeployment(job)
	if err != nil {
		return err
	}

	depclient := e.clientset.AppsV1().Deployments(e.viceNamespace)

	_, err = depclient.Get(job.InvocationID, metav1.GetOptions{})
	if err != nil {
		_, err = depclient.Create(deployment)
		if err != nil {
			return err
		}
	} else {
		_, err = depclient.Update(deployment)
		if err != nil {
			return err
		}
	}

	// Create the service for the job.
	svc := e.createService(job, deployment)

	outputstring, _ := json.Marshal(svc)
	log.Info(string(outputstring))

	svcclient := e.clientset.CoreV1().Services(e.viceNamespace)

	_, err = svcclient.Get(job.InvocationID, metav1.GetOptions{})
	if err != nil {
		_, err = svcclient.Create(&svc)
		if err != nil {
			return err
		}
	}

	return nil
}

func (e *ExposerApp) LaunchApp(writer http.ResponseWriter, request *http.Request) {
	job := &model.Job{}

	buf, err := ioutil.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	if err = json.Unmarshal(buf, job); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	if strings.ToLower(job.ExecutionTarget) != "interapps" {
		http.Error(
			writer,
			fmt.Errorf("job type %s is not supported by this service", job.Type).Error(),
			http.StatusBadRequest,
		)
		return
	}

	// Create the excludes file ConfigMap for the job.
	if err = e.UpsertExcludesConfigMap(job); err != nil {
		if err != nil {
			http.Error(
				writer,
				err.Error(),
				http.StatusInternalServerError,
			)
			return
		}
	}

	// Create the input path list config map
	if err = e.UpsertInputPathListConfigMap(job); err != nil {
		if err != nil {
			http.Error(
				writer,
				err.Error(),
				http.StatusInternalServerError,
			)
			return
		}
	}

	// Create the deployment for the job.
	if err = e.UpsertDeployment(job); err != nil {
		if err != nil {
			http.Error(
				writer,
				err.Error(),
				http.StatusInternalServerError,
			)
			return
		}
	}
}
