// Copyright (c) 2017 Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"strconv"
	"time"

	"github.com/beevik/etree"
	"github.com/bndr/gojenkins"
)

func getJenkins() (*gojenkins.Jenkins, *AppError) {
	jenkins, err := gojenkins.CreateJenkins(Cfg.JenkinsURL, Cfg.JenkinsUsername, Cfg.JenkinsPassword).Init()
	if err != nil {
		return nil, NewError("Unable to connect to jenkins!", err)
	}
	return jenkins, nil
}

func CutRelease(release string, rc string, isFirstMinorRelease bool, backportRelease bool) *AppError {
	shortRelease := release[:len(release)-2]
	releaseBranch := "release-" + shortRelease
	fullRelease := release + "-" + rc
	rcpart := rc
	if rc == "" {
		rcpart = ""
		fullRelease = release
	} else {
		rcpart = "-" + rc
	}

	isFirstMinorReleaseStr := "false"
	if isFirstMinorRelease {
		isFirstMinorReleaseStr = "true"
	}

	if err := RunReleasePrechecks(); err != nil {
		return err
	}

	// We want to return so the user knows the build has started.
	// Build jobs shoudl report their own failure.
	go func() {
		if result, err := RunJobWaitForResult(
			Cfg.ReleaseJob,
			map[string]string{
				"MM_VERSION":             release,
				"MM_RC":                  rcpart,
				"IS_FIRST_MINOR_RELEASE": isFirstMinorReleaseStr,
			}); err != nil || result != gojenkins.STATUS_SUCCESS {
			return
		}

		// Only update the CI servers and pre-release if this is the latest release
		if !backportRelease {
			SetCIServerBranch(releaseBranch)

			RunJobParameters("build-pushes/job/release-gitlab.mattermost.com", map[string]string{"MM_VERSION": fullRelease})

			SetPreReleaseTarget(fullRelease)
			RunJob(Cfg.PreReleaseJob)
		}
	}()

	return nil
}

func RunReleasePrechecks() *AppError {
	if result, err := RunJobWaitForResult(Cfg.PreChecksJob, nil); err != nil || result != gojenkins.STATUS_SUCCESS {
		return NewError("Pre-checks failed! (Did you update the database upgrade code?) Result: "+result, err)
	}

	return nil
}

func getJob(name string) (*gojenkins.Job, *AppError) {
	jenkins, err := getJenkins()
	if err != nil {
		return nil, err
	}

	if job, err := jenkins.GetJob(name); err != nil {
		return nil, NewError("Unable to get job", err)
	} else {
		return job, nil
	}

}

func GetJobConfig(name string) (string, *AppError) {
	if job, err := getJob(name); err != nil {
		return "", err
	} else {
		if config, err := job.GetConfig(); err != nil {
			return "", NewError("Unable to get job config", err)
		} else {
			return config, nil
		}
	}
}

func SaveJobConfig(name string, config string) *AppError {
	if job, err := getJob(name); err != nil {
		return err
	} else {
		err2 := job.UpdateConfig(config)
		if err2 != nil {
			return NewError("Unable to update job config", err)
		}
	}

	return nil
}

func SetCIServerBranch(branch string) *AppError {
	for _, serverjob := range Cfg.CIServerJobs {
		if config, err := GetJobConfig(serverjob); err != nil {
			return err
		} else {
			jConfig := etree.NewDocument()
			if err := jConfig.ReadFromString(config); err != nil {
				return NewError("Unable to read job configuration for "+serverjob, err)
			}

			// Change branch to build from
			element := jConfig.Root().FindElement("./properties/hudson.model.ParametersDefinitionProperty/parameterDefinitions/hudson.model.StringParameterDefinition/defaultValue")
			if element == nil {
				return NewError("Unable to correct default branch element for "+serverjob, nil)
			}
			element.SetText(branch)

			// Change build trigger
			element2 := jConfig.Root().FindElement("./triggers/jenkins.triggers.ReverseBuildTrigger/upstreamProjects")
			if element2 == nil {
				return NewError("Unable to correct build trigger element for "+serverjob, nil)
			}
			if branch == "master" {
				element2.SetText("mattermost-enterprise")
			} else {
				element2.SetText("mattermost-platform/" + branch)
			}

			jConfigStringOut, err := jConfig.WriteToString()
			if err != nil {
				return NewError("Unable to write out final job config for "+serverjob, err)
			}

			if err := SaveJobConfig(serverjob, jConfigStringOut); err != nil {
				return NewError("Unable to save job for "+serverjob, err)
			}
		}
	}

	return nil
}

func RunJob(name string) *AppError {
	return RunJobParameters(name, nil)
}

func RunJobWaitForResult(name string, parameters map[string]string) (string, *AppError) {
	job, err := getJob(name)
	if err != nil {
		return "", err
	}

	newBuildNumber := job.Raw.NextBuildNumber

	_, err2 := job.InvokeSimple(parameters)
	if err2 != nil {
		return "", NewError("Unable to envoke job.", err)
	}

	var err3 error
	var status int
	tries := 1
	build := gojenkins.Build{
		Jenkins: job.Jenkins,
		Job:     job,
		Raw:     new(gojenkins.BuildResponse),
		Depth:   1,
		Base:    "/job/" + name + "/" + strconv.FormatInt(newBuildNumber, 10),
	}
	status, err3 = build.Poll()

	for ; err3 != nil || status != 200; tries += 1 {
		status, err3 = build.Poll()
		if tries >= 5 {
			return "", NewError("Unable to get build for pre-checks job: "+strconv.Itoa(int(newBuildNumber)), err3)
		}
		time.Sleep(time.Second * time.Duration(tries))
	}

	// Wait for the build to finish
	time.Sleep(time.Second * 5)
	build.Poll()
	for build.IsRunning() {
		time.Sleep(time.Second)
		build.Poll()
	}

	return build.GetResult(), nil
}

func RunJobParameters(name string, parameters map[string]string) *AppError {
	if job, err := getJob(name); err != nil {
		return err
	} else {
		_, err2 := job.InvokeSimple(parameters)
		if err2 != nil {
			return NewError("Unable to envoke job.", err)
		}
	}

	return nil
}

func SetPreReleaseTarget(target string) *AppError {
	if config, err := GetJobConfig(Cfg.PreReleaseJob); err != nil {
		return err
	} else {
		jConfig := etree.NewDocument()
		if err := jConfig.ReadFromString(config); err != nil {
			return NewError("Unable to read job configuration for pre-release", err)
		}

		// Change target to upload
		element := jConfig.Root().FindElement("./properties/hudson.model.ParametersDefinitionProperty/parameterDefinitions/hudson.model.StringParameterDefinition/defaultValue")
		if element == nil {
			return NewError("Unable to find element for pre-release target", nil)
		}
		element.SetText(target)

		jConfigStringOut, err := jConfig.WriteToString()
		if err != nil {
			return NewError("Unable to write out final job config for pre-release job", err)
		}

		if err := SaveJobConfig(Cfg.PreReleaseJob, jConfigStringOut); err != nil {
			return NewError("Unable to save job for pre-release", err)
		}
	}

	return nil
}

func LoadtestKube(buildTag string, length int, delay int) *AppError {
	RunJobParameters(Cfg.KubeDeployJob, map[string]string{
		"BUILD_TAG":           buildTag,
		"KUBE_BRANCH":         "master",
		"KUBE_CONFIG_FILE":    "values_loadtest.yaml",
		"TEST_LENGTH_MINUTES": strconv.Itoa(length),
		"PPROF_DELAY":         strconv.Itoa(delay),
	})
	return nil
}