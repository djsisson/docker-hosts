Update /etc/hosts file with all docker container IP & DNS names
    
    
    docker run -d \
        -v /var/run/docker.sock:/tmp/docker.sock \
        -v /etc/hosts:/tmp/hosts \
        djsisson/docker-hosts
