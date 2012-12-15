# this app will clone git repos, some of them maybe from gitolite. 
set -xe

H=$STACKATO_APP_ROOT
mkdir -p $H/.ssh
mv srid-gitolite-readonly $H/.ssh/id_rsa
chmod 0600 $H/.ssh/id_rsa
