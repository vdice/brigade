// ============================================================================
// NOTE: This is the actual brigade.js file for testing the Brigade project.
// Be careful when editing!
// ============================================================================
const { events, Job, Group } = require("brigadier");
const { Check, KindJob } = require("@brigadecore/brigade-utils");

const projectName = "brigade";
const projectOrg = "brigadecore";

// Go build defaults
const goImg = "quay.io/deis/lightweight-docker-go:v0.7.0";
const gopath = "/go";
const localPath = gopath + `/src/github.com/${projectOrg}/${projectName}`;

// JS build defaults
const jsImg = "node:12.3.1-stretch";

const releaseTagRegex = /^refs\/tags\/(v[0-9]+(?:\.[0-9]+)*(?:\-.+)?)$/;

const noopJob = { run: () => { return Promise.resolve() } };

function goTest() {
  // Create a new job to run Go tests
  var job = new Job("test-go", goImg);
  job.mountPath = localPath;
  // Set a few environment variables.
  job.env = {
    "SKIP_DOCKER": "true"
  };
  // Run Go unit tests
  job.tasks = [
    `cd ${localPath}`,
    "make verify-vendored-code lint test-unit"
  ];
  return job;
}

function jsTest() {
  // Create a new job to run JS-based Brigade worker tests
  var job = new Job("test-javascript", jsImg);
  // Set a few environment variables.
  job.env = {
    "SKIP_DOCKER": "true"
  };
  job.tasks = [
    "cd /src",
    "make verify-vendored-code-js test-js yarn-audit"
  ];
  return job;
}

function e2e() {
  // Create a new job to run kind-based e2e tests
  let kind = new KindJob("test-e2e");
  // Add golang path setup as e2e script invokes the brig cli
  // by its main.go filepath
  kind.mountPath = localPath;
  kind.tasks.push(
    `cd ${localPath}`,
    "CREATE_KIND=false make e2e"
  );
  return kind;
}

function buildAndPublishImages(project, version) {
  let dockerRegistry = project.secrets.dockerhubRegistry || "docker.io";
  let dockerOrg = project.secrets.dockerhubOrg || "brigadecore";
  var job = new Job("build-and-publish-images", "docker:stable-dind");
  job.privileged = true;
  job.tasks = [
    "apk add --update --no-cache make git",
    "dockerd-entrypoint.sh &",
    "sleep 20",
    "cd /src",
    `docker login ${dockerRegistry} -u ${project.secrets.dockerhubUsername} -p ${project.secrets.dockerhubPassword}`,
    `DOCKER_REGISTRY=${dockerRegistry} DOCKER_ORG=${dockerOrg} VERSION=${version} make build-all-images push-all-images`,
    `docker logout ${dockerRegistry}`
  ];
  return job;
}

// Here we can add additional Check Runs, which will run in parallel and
// report their results independently to GitHub
function runSuite(e, p) {
  // Important: To prevent Promise.all() from failing fast, we catch and
  // return all errors. This ensures Promise.all() always resolves. We then
  // iterate over all resolved values looking for errors. If we find one, we
  // throw it so the whole build will fail.
  //
  // Ref: https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/Promise/all#Promise.all_fail-fast_behaviour
  //
  // Note: as provided language string is used in job naming, it must consist
  // of lowercase letters and hyphens only (per Brigade/K8s restrictions)
  return Promise.all([
    runTests(e, p, goTest).catch((err) => { return err }),
    runTests(e, p, jsTest).catch((err) => { return err }),
    runTests(e, p, e2e).catch((err) => { return err }),
  ])
    .then((values) => {
      values.forEach((value) => {
        if (value instanceof Error) throw value;
      });
    })
    .then(() => {
      if (e.revision.ref == "master") {
        // This builds and publishes "edge" images
        buildAndPublishImages(p, "").run();
      }
    });
}

// runCheck is the default function invoked on a check_run:* event
//
// It determines which check is being requested (from the payload body)
// and runs this particular check, or else throws an error if the check
// is not found
function runCheck(e, p) {
  payload = JSON.parse(e.payload);

  // Extract the check name
  name = payload.body.check_run.name;

  // Determine which check to run
  switch (name) {
    case "test-go":
      return runTests(e, p, goTest);
    case "test-javascript":
      return runTests(e, p, jsTest);
    case "test-e2e":
      return runTests(e, p, e2e);
    default:
      throw new Error(`No check found with name: ${name}`);
  }
}

// runTests is a Check Run that is run as part of a Checks Suite
function runTests(e, p, jobFunc) {
  console.log("Check requested");

  var check = new Check(e, p, jobFunc(),
    `https://brigadecore.github.io/kashti/builds/${e.buildID}`);
  return check.run();
}

function githubRelease(p, tag) {
  if (!p.secrets.ghToken) {
    throw new Error("Project must have 'secrets.ghToken' set");
  }
  // Cross-compile binaries for a given release and upload them to GitHub.
  var job = new Job("release", goImg);
  job.shell = "/bin/bash";
  job.mountPath = localPath;
  parts = p.repo.name.split("/", 2);
  // Set a few environment variables.
  job.env = {
    "SKIP_DOCKER": "true",
    "GITHUB_USER": parts[0],
    "GITHUB_REPO": parts[1],
    "GITHUB_TOKEN": p.secrets.ghToken,
  };
  job.tasks = [
    "go get -u github.com/tcnksm/ghr",
    `cd ${localPath}`,
    `VERSION=${tag} make build-brig`,
    `last_tag=$(git describe --tags ${tag}^ --abbrev=0 --always)`,
    `ghr \
      -u \${GITHUB_USER} \
      -r \${GITHUB_REPO} \
      -n "${parts[1]} ${tag}" \
      -b "$(git log --no-merges --pretty=format:'- %s %H (%aN)' HEAD ^$last_tag)" \
      ${tag} bin`,
    `echo "Release is at https://github.com/${p.repo.name}/releases/tag/${tag}"`
  ];
  return job;
}

function slackNotify(title, msg, project) {
  if (project.secrets.SLACK_WEBHOOK) {
    var job = new Job(`${projectName}-slack-notify`, "technosophos/slack-notify:latest");
    job.env = {
      "SLACK_WEBHOOK": project.secrets.SLACK_WEBHOOK,
      "SLACK_USERNAME": "brigade-ci",
      "SLACK_TITLE": title,
      "SLACK_MESSAGE": msg,
      "SLACK_COLOR": "#00ff00"
    };
    job.tasks = ["/slack-notify"];
    return job;
  }
  console.log(`Slack Notification for '${title}' not sent; no SLACK_WEBHOOK secret found.`);
  return noopJob;
}

////////////////////////////////////////////////////////////////////////////////////////////
// Event Handlers
////////////////////////////////////////////////////////////////////////////////////////////

events.on("e2e", () => {
  return e2e().run();
})

events.on("exec", (e, p) => {
  return Group.runAll([
    goTest(),
    jsTest(),
    e2e()
  ]);
});

events.on("push", (e, p) => {
  let matchStr = e.revision.ref.match(releaseTagRegex);
  if (matchStr) {
    // This is an official release with a semantically versioned tag
    let matchTokens = Array.from(matchStr);
    let version = matchTokens[1];
    return Group.runAll([
      goTest(),
      jsTest(),
      e2e()
    ])
      .then(() => {
        Group.runEach([
          buildAndPublishImages(p, version),
          githubRelease(p, version),
          slackNotify(
            "Brigade Release",
            `${version} release now on GitHub! <https://github.com/${p.repo.name}/releases/tag/${version}>`,
            p
          )
        ])
      });
  } else {
    if (e.revision.ref.startsWith('refs/tags')) {
      console.log(`Ref ${e.revision.ref} does not match expected official release tag regex (${releaseTagRegex}); not releasing.`);
    }
  }
})

events.on("check_suite:requested", runSuite);
events.on("check_suite:rerequested", runSuite);
events.on("check_run:rerequested", runCheck);
events.on("issue_comment:created", (e, p) => Check.handleIssueComment(e, p, runSuite));
events.on("issue_comment:edited", (e, p) => Check.handleIssueComment(e, p, runSuite));
