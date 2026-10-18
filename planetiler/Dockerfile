# syntax=docker/dockerfile:1
# The planet bake: Planetiler's OpenMapTiles profile with one change, OSM land
# cover from z5 instead of z7 (landcover-low-zoom.patch; see README "Land cover
# below z7"). Built and published by CI as ghcr.io/<owner>/streetcryptid-planetiler,
# a drop-in for ghcr.io/onthegomap/planetiler in the bake CronJob: same
# arguments, same output.

# --- build stage -------------------------------------------------------------
FROM eclipse-temurin:25-jdk-noble AS build

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# Pinned to the planetiler-openmaptiles commit that builds against Planetiler
# 0.10.2, the engine the stock image we replace ran. Bump deliberately, and
# re-check that the patch still applies and still means the same thing.
ARG OMT_REPO=https://github.com/openmaptiles/planetiler-openmaptiles.git
ARG OMT_COMMIT=4183c01cb31571d9481d3d316e584df53f989e3f

WORKDIR /src
RUN git init -q . \
 && git remote add origin "$OMT_REPO" \
 && git fetch -q --depth 1 origin "$OMT_COMMIT" \
 && git checkout -q FETCH_HEAD

COPY landcover-low-zoom.patch /tmp/landcover-low-zoom.patch
RUN git apply --verbose /tmp/landcover-low-zoom.patch

# The landcover tests the patch rewrites run here, so a patch that applies but
# no longer does what it says cannot be published. The rest of upstream's suite
# is upstream's to keep green.
RUN ./mvnw -B -q -Dtest=LandcoverTest -Dsurefire.failIfNoSpecifiedTests=false package \
 && cp target/planetiler-openmaptiles-*-with-deps.jar /out.jar

# --- final stage -------------------------------------------------------------
FROM eclipse-temurin:25-jre-noble

COPY --from=build /out.jar /app/planetiler.jar

# Runs as an arbitrary non-root uid in the cluster; every path it writes is
# passed on the command line (--tmpdir, --download-dir, --output).
USER 65532:65532
ENTRYPOINT ["java", "-jar", "/app/planetiler.jar"]
