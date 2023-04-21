# -*- mode: ruby -*-
# vi: set ft=ruby :

Vagrant.configure("2") do |config|
  # TODO: we probably want to build our own base box here, but that's... work. (tvd, 2022-10-06)
  config.vm.box = "generic/ubuntu1804"
  config.vm.box_version = "4.1.14"

  config.vm.network "forwarded_port", guest: 2375, host: 2375
  config.vm.network "forwarded_port", guest: 8080, host: 8080

  config.vm.synced_folder ".", "/home/vagrant/rchab",
    type: "nfs", mount_options: ['local_lock=all']

  config.vm.provider :libvirt do |lv|
    lv.memory = 4096
    lv.cpus = 2
  end

  config.vm.provision "shell", inline: <<-SHELL
    set -eo pipefail
    echo "Provisioning with user: $(whoami)"
    apt update -y

    apt install -y apt-transport-https ca-certificates curl software-properties-common
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo apt-key add -
    add-apt-repository "deb [arch=amd64] https://download.docker.com/linux/ubuntu bionic stable"
    apt-cache policy docker-ce

    apt install -y \
      build-essential \
      dnsutils \
      docker-ce \
      htop \
      iproute2 \
      net-tools \
      ;

    goversion=1.19.2
    wget https://go.dev/dl/go${goversion}.linux-amd64.tar.gz
    echo "5e8c5a74fe6470dd7e055a461acda8bb4050ead8c2df70f227e3ff7d8eb7eeb6 go${goversion}.linux-amd64.tar.gz" | sha256sum --check
    rm -rf /usr/local/go && tar -C /usr/local -xzf go${goversion}.linux-amd64.tar.gz
    echo 'export PATH=/usr/local/go/bin:$PATH' | tee /etc/profile.d/golang.sh
    /usr/local/go/bin/go version

    adduser vagrant docker

    # level3 failed to resolve some of the go domains when setting this up; use quad9
    sed -i 's/DNS=4.2.2.1 4.2.2.2 208.67.220.220/DNS=9.9.9.9 149.112.112.112/' /etc/systemd/resolved.conf
    systemctl restart systemd-resolved

    # disable docker; we need the software, but we run the service ourselves
    systemctl stop docker
    systemctl stop docker.socket
    systemctl disable docker
    systemctl disable docker.socket
  SHELL
end