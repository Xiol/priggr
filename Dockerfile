FROM centos:7
RUN yum -y update
RUN useradd -r -d /db --uid 950 priggr
RUN mkdir /db && chown 950:950 /db && chmod 0700 /db
COPY priggr /usr/local/bin/priggr
COPY static/ /var/www/static
USER priggr
VOLUME /db
WORKDIR /db
EXPOSE 8998
CMD /usr/local/bin/priggr -l info -d /db/priggr.db -b 0.0.0.0 -p 8998 -a /var/www/static
