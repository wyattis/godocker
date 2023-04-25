# godocker
Use ephemeral docker containers for testing.

## Usage
```go
func TestThingWithDockerfile(t *testing.T) {
  err := godocker.Run(
    godocker.WithDockerfile("ssh.Dockerfile"),
    godocker.CleanupImage(),      // cleanup docker images afterward
    godocker.CleanupContainer(),  // cleanup docker containers afterward
    godocker.Exec(func (c godocker.Config) (err error) {
      // Run your tests with the docker container
      return
    }),
  )
  if err != nil {
    t.Fatal(err)
  }
}

func TestInImage (t *testing.T) {
  err := godocker.Run(
    godocker.WithImage("mysql:latest"),
    godocker.CleanupContainer(),
    godocker.Exec(func(c godocker.Config) (err error) {
      // Run some queries against the mysql container
      return
    }),
  )
  if err != nil {
    t.Fatal(err)
  }
}
```
