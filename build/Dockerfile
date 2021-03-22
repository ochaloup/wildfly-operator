FROM registry.access.redhat.com/ubi8/ubi-minimal:latest

ENV OPERATOR=/usr/local/bin/wildfly-operator \
    JBOSS_HOME=/opt/eap \
    JBOSS_BOOTABLE_HOME=/opt/jboss/container/wildfly-bootable-jar-server \
    JBOSS_BOOTABLE_DATA_DIR=/opt/jboss/container/wildfly-bootable-jar-data \
    USER_UID=1001 \
    USER_NAME=wildfly-operator \
    LABEL_APP_MANAGED_BY=eap-operator \
    LABEL_APP_RUNTIME=eap \
    SERVER_LIVENESS_SCRIPT=/opt/eap/bin/livenessProbe.sh \
    SERVER_READINESS_SCRIPT=/opt/eap/bin/readinessProbe.sh

# install operator binary
COPY build/_output/bin/wildfly-operator ${OPERATOR}

COPY build/bin /usr/local/bin
RUN  /usr/local/bin/user_setup

ENTRYPOINT ["/usr/local/bin/entrypoint"]

USER ${USER_UID}
