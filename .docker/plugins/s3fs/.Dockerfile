FROM alpine:3

RUN apk add --no-cache gcompat xfsprogs e2fsprogs ca-certificates fuse alpine-sdk automake autoconf libxml2-dev fuse-dev curl-dev

RUN git clone https://github.com/s3fs-fuse/s3fs-fuse.git && cd s3fs-fuse && ./autogen.sh \
    && ./configure --prefix=/usr && make && make install && cd .. && rm -rf s3fs-fuse

RUN mkdir -p /etc/rexray /run/docker/plugins /var/lib/rexray/volumes
ADD rexray /usr/bin/rexray
ADD rexray.yml /etc/rexray/rexray.yml

ADD rexray.sh /rexray.sh
RUN chmod +x /rexray.sh

CMD [ "rexray", "start", "--nopid" ]
ENTRYPOINT [ "/rexray.sh" ]
